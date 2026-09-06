package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// BroadcastMsg はHubのbroadcastチャンネルで運ぶデータ
// チャットの文字列だけでなく、誰が送ったか（UserID・Username）も一緒に持つ
type BroadcastMsg struct {
	Content  string
	UserID   uint
	Username string
}

// WSMessage はWebSocketでやり取りするJSONメッセージの共通フォーマット
// typeフィールドで「chat / userCount / delete / me」を区別する
type WSMessage struct {
	Type      string `json:"type"`
	ID        uint   `json:"id,omitempty"`        // メッセージID（削除時に使う）
	Content   string `json:"content,omitempty"`   // チャット本文
	UserID    uint   `json:"userID,omitempty"`    // 送信者のユーザーID
	Username  string `json:"username,omitempty"`  // 送信者のユーザー名（表示用）
	Count     int    `json:"count,omitempty"`     // オンライン人数
	CreatedAt string `json:"createdAt,omitempty"` // 投稿時刻（DBの保存時刻をそのまま文字列で渡す）
}

// RoomView はホーム画面（/rooms）がJSONで返す部屋1件分の情報
type RoomView struct {
	ID          uint   `json:"id"`
	Name        string `json:"name"`
	OnlineCount int    `json:"onlineCount"`
}

func main() {
	r := gin.Default()

	store := cookie.NewStore([]byte(os.Getenv("session_secret")))
	r.Use(sessions.Sessions("session", store))

	r.Static("/static", "./static")

	r.Any("/register", registerHandler)
	r.Any("/login", loginHandler)
	r.Any("/logout", logoutHandler)

	wsupgrader := websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
	}

	authorized := r.Group("/")
	authorized.Use(authRequired)
	{
		// ホーム画面：部屋一覧ページ
		authorized.GET("/", func(c *gin.Context) {
			http.ServeFile(c.Writer, c.Request, "templates/home.html")
		})

		// 部屋一覧をJSONで返す（home.htmlのJSがfetchする）
		authorized.GET("/rooms", func(c *gin.Context) {
			var rooms []Room
			db.Order("created_at asc").Find(&rooms)

			views := make([]RoomView, 0, len(rooms))
			for _, room := range rooms {
				hubsMu.Lock()
				h, ok := hubs[room.ID]
				hubsMu.Unlock()

				onlineCount := 0
				if ok {
					// h.clientsはHubのgoroutineしか直接触らないので、
					// ここではチャンネル経由(OnlineCount)で安全に問い合わせる
					onlineCount = h.OnlineCount()
				}

				views = append(views, RoomView{ID: room.ID, Name: room.Name, OnlineCount: onlineCount})
			}

			c.JSON(http.StatusOK, views)
		})

		// 部屋を新規作成する
		authorized.POST("/rooms", func(c *gin.Context) {
			name := c.PostForm("name")
			if name == "" {
				c.String(http.StatusBadRequest, "部屋名を入力してください")
				return
			}

			session := sessions.Default(c)
			currentUserID := session.Get("userID").(uint)

			result := db.Create(&Room{Name: name, CreatedBy: currentUserID})
			if result.Error != nil {
				c.String(http.StatusBadRequest, "その部屋名は既に使用されています")
				return
			}

			c.Redirect(http.StatusFound, "/")
		})

		// 部屋ごとのチャット画面
		authorized.GET("/rooms/:id", func(c *gin.Context) {
			roomID, err := strconv.ParseUint(c.Param("id"), 10, 64)
			if err != nil {
				c.String(http.StatusBadRequest, "不正な部屋IDです")
				return
			}

			var room Room
			db.First(&room, roomID)
			if room.ID == 0 {
				c.String(http.StatusNotFound, "部屋が見つかりません")
				return
			}

			http.ServeFile(c.Writer, c.Request, "templates/chat.html")
		})

		// 部屋ごとのWebSocket接続
		authorized.GET("/rooms/:id/ws", func(c *gin.Context) {
			roomID, err := strconv.ParseUint(c.Param("id"), 10, 64)
			if err != nil {
				c.String(http.StatusBadRequest, "不正な部屋IDです")
				return
			}

			var room Room
			db.First(&room, roomID)
			if room.ID == 0 {
				c.String(http.StatusNotFound, "部屋が見つかりません")
				return
			}

			conn, err := wsupgrader.Upgrade(c.Writer, c.Request, nil)
			if err != nil {
				log.Printf("Failed to set websocket upgrade: %+v\n", err)
				return
			}

			// セッションからログイン中のユーザーIDを取得
			// このIDを使って「自分のメッセージかどうか」を判定する
			session := sessions.Default(c)
			currentUserID := session.Get("userID").(uint)

			// DBからユーザー名を取得（メッセージに添えて表示するため）
			var currentUser User
			db.First(&currentUser, currentUserID)

			hub := getOrCreateHub(room.ID)
			client := &Client{
				hub:      hub,
				conn:     conn,
				send:     make(chan []byte, 256),
				userID:   currentUserID,
				username: currentUser.Username,
			}

			// ① 自分のユーザーIDをブラウザに教える（削除ボタンの表示判定に使う）
			// この時点ではwritePumpがまだ動いていないので、直接書き込んでも安全
			meMsg, _ := json.Marshal(WSMessage{Type: "me", UserID: currentUserID})
			conn.WriteMessage(websocket.TextMessage, meMsg)

			// ② この部屋の過去のチャット履歴を送る
			var chatHistory []ChatMessage
			db.Where("room_id = ?", room.ID).Order("created_at asc").Find(&chatHistory)
			for _, m := range chatHistory {
				msg, _ := json.Marshal(WSMessage{
					Type:      "chat",
					ID:        m.ID,
					Content:   m.Content,
					UserID:    m.UserID,
					Username:  m.Username,
					CreatedAt: m.CreatedAt.Format("2006/01/02 15:04:05"),
				})
				conn.WriteMessage(websocket.TextMessage, msg)
			}

			// ③ 書き込み・読み取り専用のgoroutineを起動してからHubに登録する
			go client.writePump()
			go client.readPump()
			hub.register <- client
		})
	}

	fmt.Println("サーバー起動: http://localhost:4001/register")
	r.Run(":4001")
}
