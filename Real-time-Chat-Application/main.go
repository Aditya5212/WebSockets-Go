package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
)

// We'll need a "Upgrader" to upgrade HTTP connections to WebSocket connections.
var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024, // Size of the read buffer in bytes
	WriteBufferSize: 1024, // Size of the write buffer in bytes
	// CheckOrigin is a function to check the origin of the request.
	// For simplicity in this example, we'll allow all origins.
	// In a production environment, you should implement proper origin checking.
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// Client represents a single chatting user.
type Client struct {
	conn *websocket.Conn // The WebSocket connection.
	send chan []byte     // Buffered channel of outbound messages.
	name string
}

type Message struct {
	Sender  string `json:"sender"`
	Content string `json:"content"`
}

// Hub maintains the set of active clients and broadcasts messages to the clients.
type Hub struct {
	clients    map[*Client]bool // Registered clients.
	broadcast  chan []byte      // Inbound messages from the clients.
	register   chan *Client     // Register requests from the clients.
	unregister chan *Client     // Unregister requests from clients.
	mu         sync.Mutex       // For thread-safe access to clients map
}

func newHub() *Hub {
	return &Hub{
		broadcast:  make(chan []byte),
		register:   make(chan *Client),
		unregister: make(chan *Client),
		clients:    make(map[*Client]bool),
	}
}

// run is the heart of the Hub, managing client registrations,
// unregistrations, and message broadcasting.
func (h *Hub) run() {
	for {
		select {
		case client := <-h.register:
			h.mu.Lock()
			h.clients[client] = true
			log.Printf("Client registered: %s", client.conn.RemoteAddr())
			h.mu.Unlock()
		case client := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				close(client.send)
				log.Printf("Client unregistered: %s", client.conn.RemoteAddr())
			}
			h.mu.Unlock()
		case message := <-h.broadcast:
			h.mu.Lock()
			// Parse the message to get the sender
			var msg Message
			if err := json.Unmarshal(message, &msg); err == nil {
				for client := range h.clients {
					// Don't send the message back to the sender
					if client.name != msg.Sender {
						select {
						case client.send <- message:
						default:
							close(client.send)
							delete(h.clients, client)
							log.Printf("Client send buffer full or disconnected, removing: %s", client.conn.RemoteAddr())
						}
					}
				}
			}
			h.mu.Unlock()
		}
	}
}

// readPump pumps messages from the WebSocket connection to the hub.
// The application runs readPump in a per-connection goroutine. The application
// ensures that there is at most one reader on a connection by executing all
// reads from this goroutine.
func (c *Client) readPump(hub *Hub) {
	defer func() { // Ensure cleanup on exit
		hub.unregister <- c
		c.conn.Close()
		log.Printf("Connection closed for readPump: %s", c.conn.RemoteAddr())
	}()
	// Set a read limit to prevent excessively large messages.
	c.conn.SetReadLimit(512) // Max message size in bytes
	// Configure a pong handler to respond to pings from the client
	// This also helps in keeping the connection alive.
	c.conn.SetPongHandler(func(string) error {
		log.Printf("Pong received from %s", c.conn.RemoteAddr())
		// You might want to extend the read deadline here if you have one set
		return nil
	})

	for {
		// ReadMessage blocks until a message is received or an error occurs.
		// It returns the message type (e.g., TextMessage, BinaryMessage),
		// the message payload (as []byte), and an error.
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("error: %v", err)
			}
			log.Printf("Read error from client %s: %v", c.conn.RemoteAddr(), err)
			break // Exit loop, which triggers the defer to unregister and close
		}

		formattedMsg, _ := json.Marshal(Message{
			Sender:  c.name,
			Content: string(message),
		})
		message = formattedMsg

		log.Printf("Received message from %s: %s", c.conn.RemoteAddr(), string(message))
		hub.broadcast <- message // Send the received message to the hub's broadcast channel.
	}
}

// writePump pumps messages from the hub to the WebSocket connection.
// A goroutine running writePump is started for each connection. The
// application ensures that there is at most one writer to a connection by
// executing all writes from this goroutine.
func (c *Client) writePump() {
	defer func() { // Ensure cleanup on exit
		c.conn.Close() // Close the WebSocket connection
		log.Printf("Connection closed for writePump: %s", c.conn.RemoteAddr())
	}()
	for {
		select {
		case message, ok := <-c.send:
			if !ok {
				// The hub closed the channel.
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				log.Printf("Hub closed channel for client %s", c.conn.RemoteAddr())
				return
			}

			// WriteMessage sends a message. It will block until the message is sent or an error occurs.
			// We are sending a TextMessage.
			err := c.conn.WriteMessage(websocket.TextMessage, message)
			if err != nil {
				log.Printf("Write error to client %s: %v", c.conn.RemoteAddr(), err)
				return // Exit loop, defer will close connection
			}
			log.Printf("Sent message to %s: %s", c.conn.RemoteAddr(), string(message))
		}
	}
}

// serveWs handles websocket requests from the peer.
func serveWs(hub *Hub, w http.ResponseWriter, r *http.Request) {
	log.Println("New WebSocket connection attempt...")
	// Get the client's name from query parameter
	name := r.URL.Query().Get("name")
	if name == "" {
		name = "Anonymous"
	}

	// Upgrade upgrades the HTTP server connection to the WebSocket protocol.
	conn, err := upgrader.Upgrade(w, r, nil) // The third argument is response headers, nil for now.
	if err != nil {
		log.Printf("Failed to upgrade to WebSocket: %v", err)
		return
	}
	log.Printf("WebSocket connection established: %s", conn.RemoteAddr())

	// Create a new client
	client := &Client{
		conn: conn,
		send: make(chan []byte, 256),
		name: name,
	} // Buffered channel for sends
	hub.register <- client // Register the new client with the hub

	// Allow collection of memory referenced by the go router potentially.
	// Start the read and write pumps as separate goroutines.
	// This allows concurrent reading and writing for this client.
	go client.writePump()
	go client.readPump(hub) // Pass the hub to readPump so it can send messages to the hub
}

func main() {
	hub := newHub()
	go hub.run() // Start the central hub in its own goroutine

	// Define an HTTP endpoint "/ws" that will handle WebSocket connections.
	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		serveWs(hub, w, r)
	})

	// Define a simple HTTP endpoint "/" to serve a basic HTML page for testing.
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "index.html") // We'll create this file next
	})

	port := "8080"
	log.Printf("Starting server on :%s", port)
	err := http.ListenAndServe(":"+port, nil) // Start the HTTP server
	if err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}
