package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

var clients = make(map[*websocket.Conn]bool)
var mu sync.Mutex

// broadcast チャンネルはチャットメッセージの中身と送信者IDをセットで運ぶ
var broadcast = make(chan BroadcastMsg)

// BroadcastMsg はbroadcastチャンネルで運ぶデータ
// チャットの文字列だけでなく、誰が送ったか（UserID）も一緒に持つ
type BroadcastMsg struct {
	Content string
	UserID  uint
}

// WSMessage はWebSocketでやり取りするJSONメッセージの共通フォーマット
// typeフィールドで「chat / userCount / delete / me」を区別する
type WSMessage struct {
	Type      string `json:"type"`
	ID        uint   `json:"id,omitempty"`        // メッセージID（削除時に使う）
	Content   string `json:"content,omitempty"`   // チャット本文
	UserID    uint   `json:"userID,omitempty"`    // 送信者のユーザーID
	Count     int    `json:"count,omitempty"`     // オンライン人数
	CreatedAt string `json:"createdAt,omitempty"` // 投稿時刻（DBの保存時刻をそのまま文字列で渡す）
}

func main() {
	r := gin.Default()

	store := cookie.NewStore([]byte("secret-key"))
	r.Use(sessions.Sessions("session", store))

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
		authorized.GET("/chat", func(c *gin.Context) {
			http.ServeFile(c.Writer, c.Request, "templates/chat.html")
		})

		authorized.GET("/ws", func(c *gin.Context) {
			conn, err := wsupgrader.Upgrade(c.Writer, c.Request, nil)
			if err != nil {
				log.Printf("Failed to set websocket upgrade: %+v\n", err)
				return
			}

			// セッションからログイン中のユーザーIDを取得
			// このIDを使って「自分のメッセージかどうか」を判定する
			session := sessions.Default(c)
			currentUserID := session.Get("userID").(uint)

			mu.Lock()
			clients[conn] = true
			mu.Unlock()

			broadcastUserCount()

			defer func() {
				mu.Lock()
				delete(clients, conn)
				mu.Unlock()
				conn.Close()
				broadcastUserCount()
			}()

			// ① 自分のユーザーIDをブラウザに教える（削除ボタンの表示判定に使う）
			meMsg, _ := json.Marshal(WSMessage{Type: "me", UserID: currentUserID})
			conn.WriteMessage(websocket.TextMessage, meMsg)

			// ② 過去のチャット履歴を送る（IDとUserIDも含める）
			var chatHistory []ChatMessage
			db.Order("created_at asc").Find(&chatHistory)
			for _, m := range chatHistory {
				msg, _ := json.Marshal(WSMessage{
					Type:      "chat",
					ID:        m.ID,
					Content:   m.Content,
					UserID:    m.UserID,
					CreatedAt: m.CreatedAt.Format("2006/01/02 15:04:05"), // DBの投稿時刻を文字列に変換
				})
				conn.WriteMessage(websocket.TextMessage, msg)
			}

			// ③ クライアントからのメッセージを待ち続けるループ
			for {
				_, rawMsg, err := conn.ReadMessage()
				if err != nil {
					break
				}

				// クライアントからのメッセージはすべてJSON形式
				var incoming WSMessage
				if err := json.Unmarshal(rawMsg, &incoming); err != nil {
					continue // JSONでなければ無視
				}

				switch incoming.Type {
				case "chat":
					// チャットメッセージ → broadcastに流してhandleMessageへ
					broadcast <- BroadcastMsg{Content: incoming.Content, UserID: currentUserID}
				case "delete":
					// 削除リクエスト → 自分のメッセージか確認してDB削除＆全員に通知
					handleDelete(incoming.ID, currentUserID)
				}
			}
		})
	}

	go handleMessage()

	fmt.Println("サーバー起動: http://localhost:4001/register")
	r.Run(":4001")
}

// broadcastUserCount は現在のオンライン人数を全クライアントに送信する
func broadcastUserCount() {
	mu.Lock()
	defer mu.Unlock()

	count := len(clients)
	msg, _ := json.Marshal(WSMessage{Type: "userCount", Count: count})
	for client := range clients {
		client.WriteMessage(websocket.TextMessage, msg)
	}
}

// handleDelete はメッセージの所有者確認・DB削除・全員への削除通知を行う
func handleDelete(msgID uint, currentUserID uint) {
	// DBからメッセージを取得
	var chatMsg ChatMessage
	db.First(&chatMsg, msgID)

	// 存在しない、または自分のメッセージでない場合は何もしない
	if chatMsg.ID == 0 || chatMsg.UserID != currentUserID {
		return
	}

	// DBから削除（GORMのDeleteはsoft delete: deleted_atに時刻を入れるだけ）
	db.Delete(&chatMsg)

	// 全クライアントに「このIDのメッセージを削除して」と通知する
	deleteMsg, _ := json.Marshal(WSMessage{Type: "delete", ID: msgID})
	mu.Lock()
	for client := range clients {
		client.WriteMessage(websocket.TextMessage, deleteMsg)
	}
	mu.Unlock()
}

// handleMessage はbroadcastチャンネルからメッセージを受け取り全員に転送する
func handleMessage() {
	for {
		bm := <-broadcast

		// DBに保存（UserIDも一緒に保存する）
		chatMsg := ChatMessage{Content: bm.Content, UserID: bm.UserID}
		db.Create(&chatMsg)

		// DB保存後のID・CreatedAtを含めてJSONを作る
		// db.Create()後は chatMsg.CreatedAt にDBが採番した時刻が入っている
		msg, _ := json.Marshal(WSMessage{
			Type:      "chat",
			ID:        chatMsg.ID,
			Content:   bm.Content,
			UserID:    bm.UserID,
			CreatedAt: chatMsg.CreatedAt.Format("2006/01/02 15:04:05"),
		})

		mu.Lock()
		for client := range clients {
			err := client.WriteMessage(websocket.TextMessage, msg)
			if err != nil {
				log.Printf("error: %v", err)
				client.Close()
				delete(clients, client)
			}
		}
		mu.Unlock()
	}
}
