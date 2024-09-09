package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/gorilla/websocket"
)

// Структуры для базы данных
type Room struct {
	ID   uint   `gorm:"primaryKey"`
	Name string `gorm:"unique"`
}

type User struct {
	ID       uint   `gorm:"primaryKey"`
	Username string `gorm:"unique"`
}

type Message struct {
	ID      uint `gorm:"primaryKey"`
	Content string
	RoomID  uint
	UserID  uint
	User    User `gorm:"foreignKey:UserID" `
	Room    Room `gorm:"foreignKey:RoomID"`
}

type ConnData struct {
	Username string `json:"username"`
	Room     string `json:"room"`
}

// Структура клиента
type Client struct {
	conn     *websocket.Conn
	username string
	room     string
}

// Глобальные переменные
var db *gorm.DB
var rooms = make(map[string]map[*Client]bool)
var broadcast = make(chan Message)
var mutex = sync.Mutex{}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// Подключение к базе данных
func initDB() {
	dsn := "host=localhost user=admin password=maxim dbname=chat port=5432 sslmode=disable"
	var err error
	db, err = gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatal("Не удалось подключиться к базе данных:", err)
	}

	// Авто-миграции (создание таблиц)
	db.AutoMigrate(&Room{}, &User{}, &Message{})
}

// Функция для создания комнаты, если её нет
func createRoomIfNotExists(roomName string) (Room, error) {
	var room Room
	if err := db.FirstOrCreate(&room, Room{Name: roomName}).Error; err != nil {
		return Room{}, err
	}
	return room, nil
}

// Функция для создания пользователя, если его нет
func createUserIfNotExists(username string) (User, error) {
	var user User
	if err := db.FirstOrCreate(&user, User{Username: username}).Error; err != nil {
		return User{}, err
	}
	return user, nil
}

// Обработчик для получения списка комнат
func getRooms(w http.ResponseWriter, r *http.Request) {
	var roomList []Room
	if err := db.Find(&roomList).Error; err != nil {
		log.Println("Ошибка получения комнат:", err)
		http.Error(w, "Не удалось получить список комнат", http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(roomList)
}

// Обработчик для получения старых сообщений
func getMessages(w http.ResponseWriter, r *http.Request) {
	roomName := r.URL.Query().Get("room")
	if roomName == "" {
		http.Error(w, "Комната не указана", http.StatusBadRequest)
		return
	}

	var room Room
	if err := db.Where("name = ?", roomName).First(&room).Error; err != nil {
		http.Error(w, "Комната не найдена", http.StatusNotFound)
		return
	}

	var messages []Message
	if err := db.Preload("User").Where("room_id = ?", room.ID).Order("created_at asc").Find(&messages).Error; err != nil {
		http.Error(w, "Не удалось получить сообщения", http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(messages)
}

// Обработчик WebSocket соединений
func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Ошибка обновления до WebSocket:", err)
		return
	}
	defer conn.Close()

	// Чтение комнаты и имени пользователя от клиента
	var connData ConnData

	err = conn.ReadJSON(&connData)
	if err != nil {
		log.Println("Ошибка чтения данных пользователя:", err)
		return
	}

	// Создание или получение комнаты и пользователя
	room, err := createRoomIfNotExists(connData.Room)
	if err != nil {
		log.Println("Ошибка создания комнаты:", err)
		return
	}

	user, err := createUserIfNotExists(connData.Username)
	if err != nil {
		log.Println("Ошибка создания пользователя:", err)
		return
	}

	client := &Client{conn: conn, username: connData.Username, room: connData.Room}

	// Добавление клиента в комнату
	mutex.Lock()
	if rooms[connData.Room] == nil {
		rooms[connData.Room] = make(map[*Client]bool)
	}
	rooms[connData.Room][client] = true
	mutex.Unlock()

	// Чтение сообщений от клиента
	for {
		_, messageContent, err := conn.ReadMessage()
		if err != nil {
			log.Println("Ошибка чтения сообщения:", err)
			mutex.Lock()
			delete(rooms[connData.Room], client)
			mutex.Unlock()
			break
		}

		// Сохранение сообщения в базе данных
		msg := Message{
			Content: string(messageContent),
			RoomID:  room.ID,
			UserID:  user.ID,
		}
		db.Create(&msg)

		var newMsg Message

		// Отправляем сообщение в канал для рассылки
		if err := db.Preload("User").Where("id = ?", msg.ID).Find(&newMsg).Error; err != nil {
			log.Println("Error when get new message from db")
			return
		}

		broadcast <- newMsg
	}
}

// Обработчик для рассылки сообщений
func handleMessages() {
	for {
		// Получаем сообщение из канала
		msg := <-broadcast

		// Получаем комнату по ID
		var room Room
		db.First(&room, msg.RoomID)

		// Рассылаем сообщение всем клиентам в комнате
		mutex.Lock()
		for client := range rooms[room.Name] {
			err := client.conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf("%s: %s", msg.User.Username, msg.Content)))
			if err != nil {
				log.Printf("Ошибка отправки сообщения клиенту: %v", err)
				client.conn.Close()
				delete(rooms[room.Name], client)
			}
		}
		mutex.Unlock()
	}
}

// Обработчик для отдачи HTML страницы
func serveHome(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	http.ServeFile(w, r, "index.html")
}

func main() {
	// Инициализация базы данных
	initDB()

	// Обработчик для WebSocket
	http.HandleFunc("/ws", handleWebSocket)

	// Эндпоинт для получения списка комнат
	http.HandleFunc("/rooms", getRooms)

	// Эндпоинт для получения старых сообщений
	http.HandleFunc("/messages", getMessages)

	// Обработчик для HTML страницы
	http.HandleFunc("/", serveHome)

	// Запуск горутины для обработки сообщений
	go handleMessages()

	// Запуск HTTP сервера
	fmt.Println("Сервер запущен на :8080")
	err := http.ListenAndServe(":8080", nil)
	if err != nil {
		log.Fatal("Ошибка запуска сервера:", err)
	}
}
