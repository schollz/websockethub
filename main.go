package main

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"time"

	guuid "github.com/google/uuid"
	"github.com/gorilla/websocket"
	log "github.com/schollz/logger"
)

func main() {
	log.SetLevel("trace")
	port := 8398
	log.Infof("listening on :%d", port)
	go h.run()
	http.HandleFunc("/", handler)
	http.ListenAndServe(fmt.Sprintf(":%d", port), nil)
}

func handler(w http.ResponseWriter, r *http.Request) {
	t := time.Now().UTC()
	err := handle(w, r)
	if err != nil {
		log.Error(err)
	}
	log.Infof("%v %v %v %s\n", r.RemoteAddr, r.Method, r.URL.Path, time.Since(t))
}

func handle(w http.ResponseWriter, r *http.Request) (err error) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS, PUT, DELETE")
	w.Header().Set("Access-Control-Allow-Headers", "Accept, Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization")

	// very special paths
	if r.URL.Path == "/ws" {
		return handleWebsocket(w, r)
	} else {
		b, _ := ioutil.ReadFile("index.html")
		w.Write(b)
	}

	return
}

type Payload struct {
	Message   string `json:"message,omitempty"`
	Data      string `json:"data,omitempty"`
	Success   bool   `json:"success"`
	Broadcast bool   `json:"broadcast,omitempty"`
}

type ChangeTime struct {
	Time     time.Time
	UnixTime int64
}

const (
	// Time allowed to write a message to the peer.
	writeWait = 10 * time.Second

	// Time allowed to read the next pong message from the peer.
	pongWait = 60 * time.Second

	// Send pings to peer with this period. Must be less than pongWait.
	pingPeriod = (pongWait * 9) / 10

	// Maximum message size allowed from peer.
	maxMessageSize = 40000000
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

// connection is an middleman between the websocket connection and the hub.
type connection struct {
	// The websocket connection.
	ws *websocket.Conn

	// Buffered channel of outbound messages.
	send chan Payload

	// random assigned ID
	id     string
	domain string
}

type message struct {
	id      string
	room    string
	payload Payload
}

type subscription struct {
	conn *connection
	room string
}

// readPump pumps messages from the websocket connection to the hub.
func (s subscription) readPump() {
	c := s.conn
	defer func() {
		h.unregister <- s
		c.ws.Close()
	}()
	c.ws.SetReadLimit(maxMessageSize)
	c.ws.SetReadDeadline(time.Now().Add(pongWait))
	c.ws.SetPongHandler(func(string) error { c.ws.SetReadDeadline(time.Now().Add(pongWait)); return nil })
	for {
		var p Payload
		err := c.ws.ReadJSON(&p)
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway) {
				log.Errorf("error: %v", err)
			}
			break
		}
		m := message{c.id, s.room, p}
		log.Debugf("[%s] %s broadcasting %d bytes", s.room, c.id, len(p.Data))
		go m.handleMessage(c)
	}
}

func (m message) handleMessage(c *connection) {
	// server can intercept messages
	if m.payload.Message == "hello" {
		m.payload = Payload{
			Message:   "hello",
			Data:      fmt.Sprintf("%s connected, %d in room", m.id, len(h.rooms[m.room])),
			Success:   true,
			Broadcast: true,
		}
	}
	h.broadcast <- m
}

// write writes a message with the given message type and payload.
func (c *connection) write(mt int, payload []byte) error {
	c.ws.SetWriteDeadline(time.Now().Add(writeWait))
	return c.ws.WriteMessage(mt, payload)
}

// writeJSON writes a message with the given message type and payload.
func (c *connection) writeJSON(payload Payload) error {
	c.ws.SetWriteDeadline(time.Now().Add(writeWait))
	return c.ws.WriteJSON(payload)
}

// writePump pumps messages from the hub to the websocket connection.
func (s *subscription) writePump() {
	c := s.conn
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.ws.Close()
	}()
	for {
		select {
		case message, ok := <-c.send:
			if !ok {
				c.write(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.writeJSON(message); err != nil {
				return
			}
		case <-ticker.C:
			if err := c.write(websocket.PingMessage, []byte{}); err != nil {
				return
			}
		}
	}
}

// handleWebsocket handles websocket requests from the peer.
func handleWebsocket(w http.ResponseWriter, r *http.Request) (err error) {
	roomQuery, ok := r.URL.Query()["domain"]
	if !ok || len(roomQuery[0]) < 1 {
		err = fmt.Errorf("no domain parameter")
		return
	}
	room := roomQuery[0]

	log.Debugf("[%s] new connection", room)
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Error(err)
		return
	}
	c := &connection{
		send:   make(chan Payload, 256),
		ws:     ws,
		id:     strings.Split(guuid.New().String(), "-")[0],
		domain: room,
	}
	s := subscription{c, room}
	h.register <- s
	go s.writePump()
	s.readPump()

	log.Debugf("[%s] finished serving", room)
	return
}

// hub maintains the set of active connections and broadcasts messages to the
// connections.
type hub struct {
	// Registered connections.
	rooms map[string]map[*connection]bool

	// Inbound messages from the connections.
	broadcast chan message

	// Register requests from the connections.
	register chan subscription

	// Unregister requests from connections.
	unregister chan subscription
}

var h = hub{
	broadcast:  make(chan message),
	register:   make(chan subscription),
	unregister: make(chan subscription),
	rooms:      make(map[string]map[*connection]bool),
}

func (h *hub) run() {
	for {
		select {
		case s := <-h.register:
			log.Debugf("[%s] registering %s", s.room, s.conn.id)
			connections := h.rooms[s.room]
			if connections == nil {
				log.Debugf("[%s] creating room", s.room)
				connections = make(map[*connection]bool)
				h.rooms[s.room] = connections
			}
			h.rooms[s.room][s.conn] = true
		case s := <-h.unregister:
			log.Debugf("[%s] unregistering %s", s.room, s.conn.id)
			connections := h.rooms[s.room]
			if connections != nil {
				if _, ok := connections[s.conn]; ok {
					delete(connections, s.conn)
					close(s.conn.send)
					if len(connections) == 0 {
						log.Debugf("[%s] deleting room", s.room)
						delete(h.rooms, s.room)
					}
				}
			}
		case m := <-h.broadcast:
			connections := h.rooms[m.room]
			for c := range connections {
				if !m.payload.Broadcast && c.id == m.id {
					continue
				}
				select {
				case c.send <- m.payload:
				default:
					close(c.send)
					delete(connections, c)
					if len(connections) == 0 {
						delete(h.rooms, m.room)
					}
				}
			}
		}
	}
}
