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
	CreatedAt string `json:"createdAt,omitempty"` // 投稿時刻（RFC 3339 の UTC。表示の形にするのはブラウザ側）
}

// RoomView はホーム画面（/rooms）がJSONで返す部屋1件分の情報
type RoomView struct {
	ID          uint   `json:"id"`
	Name        string `json:"name"`
	OnlineCount int    `json:"onlineCount"`
	IsOwner     bool   `json:"isOwner"` // 自分が作成した部屋か（削除ボタンの表示判定に使う）
}

func main() {
	r := gin.Default()
	r.LoadHTMLGlob("templates/*.html") // templates フォルダの HTML を、起動時にテンプレートとして読み込む

	// session_secret はログイン状態の Cookie に署名する鍵。空のまま起動すると、
	// 誰でも同じ「空の鍵」で Cookie を作れてしまい、他人になりすませる。
	// 設定忘れに気づけるよう、空なら起動しない
	sessionSecret := os.Getenv("session_secret")
	if sessionSecret == "" {
		log.Fatal("環境変数 session_secret が設定されていません。.env を確認してください")
	}
	if len(sessionSecret) < 32 {
		log.Println("警告: session_secret が短すぎます。32文字以上のランダムな文字列にしてください")
	}

	store := cookie.NewStore([]byte(sessionSecret))
	r.Use(sessions.Sessions("session", store))

	store.Options(sessions.Options{
		Path:     "/",
		MaxAge:   86400 * 30, // 30日
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	// CSS などの静的ファイル。ブラウザが古い CSS を覚えたまま使い続けないよう、
	// 毎回「新しくなっていないか」をサーバーに確認させる（no-cache）。
	// 変わっていなければ 304（中身なし）が返るだけなので、通信量はほとんど増えない
	static := r.Group("/static", func(c *gin.Context) {
		c.Header("Cache-Control", "no-cache")
	})
	static.Static("/", "./static")

	r.Any("/register", registerHandler)
	r.Any("/login", loginHandler)
	r.POST("/logout", logoutHandler) // ログアウトは POST のみ（理由は auth.go の logoutHandler を参照）

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

			session := sessions.Default(c)
			currentUserID := session.Get("userID").(uint)

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

				views = append(views, RoomView{
					ID:          room.ID,
					Name:        room.Name,
					OnlineCount: onlineCount,
					IsOwner:     room.CreatedBy == currentUserID,
				})
			}

			c.JSON(http.StatusOK, views)
		})

		// 部屋を新規作成する
		authorized.POST("/rooms", func(c *gin.Context) {
			name := c.PostForm("name")
			if name == "" {
				c.String(http.StatusBadRequest, "プロジェクト名を入力してください")
				return
			}

			session := sessions.Default(c)
			currentUserID := session.Get("userID").(uint)

			result := db.Create(&Room{Name: name, CreatedBy: currentUserID})
			if result.Error != nil {
				c.String(http.StatusBadRequest, "そのプロジェクト名は既に使用されています")
				return
			}

			c.Redirect(http.StatusFound, "/")
		})

		// 部屋を削除する（作成者本人のみ）
		authorized.DELETE("/rooms/:id", func(c *gin.Context) {
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

			session := sessions.Default(c)
			currentUserID := session.Get("userID").(uint)
			if room.CreatedBy != currentUserID {
				c.String(http.StatusForbidden, "この部屋を削除できるのは作成者だけです")
				return
			}

			// 接続中のユーザーがいれば、部屋が削除されたことを知らせる
			// （知らせを受けたクライアント側がWebSocketを閉じてホームへ戻る）
			hubsMu.Lock()
			h, ok := hubs[room.ID]
			hubsMu.Unlock()
			if ok {
				h.NotifyRoomDeleted()
			}

			db.Where("room_id = ?", room.ID).Delete(&ChatMessage{})
			db.Delete(&room)

			c.Status(http.StatusNoContent)
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

			c.HTML(http.StatusOK, "chat.html", gin.H{"RoomName": room.Name})
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
					CreatedAt: formatWireTime(m.CreatedAt),
				})
				conn.WriteMessage(websocket.TextMessage, msg)
			}

			// ③ 書き込み・読み取り専用のgoroutineを起動してからHubに登録する
			go client.writePump()
			go client.readPump()
			hub.register <- client
		})
	}

	fmt.Println("DevChat 起動: http://localhost:4001/register")
	r.Run(":4001")
}
