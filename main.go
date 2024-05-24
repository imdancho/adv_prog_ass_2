package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

type Client struct {
	conn     *websocket.Conn
	send     chan []byte
	room     *Room
	userID   string
	username string
}

type Room struct {
	clients    map[*Client]bool
	broadcast  chan []byte
	register   chan *Client
	unregister chan *Client
	messages   []Message
}

type Message struct {
	UserID   string `json:"userId"`
	Username string `json:"username"`
	Message  string `json:"message"`
}

type User struct {
	UserID   string `json:"userId"`
	Username string `json:"username"`
	Active   bool   `json:"active"`
}

var rooms = make(map[string]*Room)
var roomsMutex sync.Mutex
var users = make(map[string]User) // userId to User struct mapping
var usersMutex sync.Mutex
var userMessages = make(map[string][]Message) // userId to messages mapping
var userMessagesMutex sync.Mutex

func (room *Room) run() {
	for {
		select {
		case client := <-room.register:
			room.clients[client] = true
			if messages, ok := userMessages[client.userID]; ok {
				for _, msg := range messages {
					client.send <- serializeMessage(msg)
				}
			}
		case client := <-room.unregister:
			if _, ok := room.clients[client]; ok {
				delete(room.clients, client)
				close(client.send)
			}
		case message := <-room.broadcast:
			var msg Message
			err := json.Unmarshal(message, &msg)
			if err != nil {
				log.Println("error decoding message:", err)
				continue
			}
			room.messages = append(room.messages, msg)

			userMessagesMutex.Lock()
			userMessages[msg.UserID] = append(userMessages[msg.UserID], msg)
			userMessagesMutex.Unlock()

			for client := range room.clients {
				select {
				case client.send <- message:
				default:
					close(client.send)
					delete(room.clients, client)
				}
			}
		}
	}
}

func serializeMessage(msg Message) []byte {
	data, _ := json.Marshal(msg)
	return data
}

func (client *Client) readPump() {
	defer func() {
		client.room.unregister <- client
		client.conn.Close()
	}()
	for {
		_, message, err := client.conn.ReadMessage()
		if err != nil {
			log.Println("read error:", err)
			break
		}
		client.room.broadcast <- message
	}
}

func (client *Client) writePump() {
	for {
		select {
		case message, ok := <-client.send:
			if !ok {
				client.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			client.conn.WriteMessage(websocket.TextMessage, message)
		}
	}
}

func serveWs(room *Room, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}

	userID := r.Header.Get("X-User-ID")
	username := r.Header.Get("X-Username")

	client := &Client{
		conn:     conn,
		send:     make(chan []byte, 256),
		room:     room,
		userID:   userID,
		username: username,
	}
	client.room.register <- client

	go client.writePump()
	go client.readPump()
}

func handleConnections(w http.ResponseWriter, r *http.Request) {
	roomID := r.URL.Query().Get("room")
	if roomID == "" {
		http.Error(w, "Room ID is required", http.StatusBadRequest)
		return
	}

	roomsMutex.Lock()
	room, ok := rooms[roomID]
	if !ok {
		room = &Room{
			clients:    make(map[*Client]bool),
			broadcast:  make(chan []byte),
			register:   make(chan *Client),
			unregister: make(chan *Client),
			messages:   []Message{},
		}
		rooms[roomID] = room
		go room.run()
	}
	roomsMutex.Unlock()

	serveWs(room, w, r)
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	username := r.URL.Query().Get("username")
	role := r.URL.Query().Get("role")
	if username == "" {
		http.Error(w, "Username is required", http.StatusBadRequest)
		return
	}

	var userId string
	var isNewUser bool

	usersMutex.Lock()
	for id, user := range users {
		if user.Username == username {
			userId = id
			break
		}
	}
	if userId == "" {
		userId = uuid.New().String()
		users[userId] = User{UserID: userId, Username: username, Active: true}
		isNewUser = true
	} else {
		user := users[userId]
		user.Active = true
		users[userId] = user
	}
	usersMutex.Unlock()

	response := map[string]string{
		"role":     role,
		"userId":   userId,
		"username": username,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)

	if isNewUser {
		userMessagesMutex.Lock()
		messages, ok := userMessages[userId]
		userMessagesMutex.Unlock()
		if ok && len(messages) > 0 {
			for _, msg := range messages {
				data := serializeMessage(msg)
				sendToUser(userId, data)
			}
		}
	}
}

func handleUsers(w http.ResponseWriter, r *http.Request) {
	usersMutex.Lock()
	defer usersMutex.Unlock()

	response := make([]User, 0, len(users))
	for _, user := range users {
		if user.Active {
			response = append(response, user)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"users": response})
}

func handleMessages(w http.ResponseWriter, r *http.Request) {
	roomID := r.URL.Query().Get("room")
	if roomID == "" {
		http.Error(w, "Room ID is required", http.StatusBadRequest)
		return
	}

	roomsMutex.Lock()
	room, ok := rooms[roomID]
	roomsMutex.Unlock()

	if !ok {
		http.Error(w, "Room not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"messages": room.messages})
}

func handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	userId := r.URL.Query().Get("userId")
	if userId == "" {
		http.Error(w, "User ID is required", http.StatusBadRequest)
		return
	}

	usersMutex.Lock()
	defer usersMutex.Unlock()

	user, ok := users[userId]
	if !ok {
		http.Error(w, "User not found", http.StatusNotFound)
		return
	}

	user.Active = false
	users[userId] = user

	userMessagesMutex.Lock()
	delete(userMessages, userId)
	userMessagesMutex.Unlock()

	w.WriteHeader(http.StatusOK)
}

func sendToUser(userId string, message []byte) {
	roomsMutex.Lock()
	defer roomsMutex.Unlock()

	for _, room := range rooms {
		for client := range room.clients {
			if client.userID == userId {
				client.send <- message
			}
		}
	}
}

func main() {
	fs := http.FileServer(http.Dir("./static"))
	http.Handle("/", fs)
	http.HandleFunc("/ws", handleConnections)
	http.HandleFunc("/login", handleLogin)
	http.HandleFunc("/users", handleUsers)
	http.HandleFunc("/messages", handleMessages)
	http.HandleFunc("/deleteUser", handleDeleteUser)
	log.Println("Server started on :8080")
	err := http.ListenAndServe(":8080", nil)
	if err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}
