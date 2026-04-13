package main

import (
	"fmt"
	"log"
	"net/http"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// mapはキーと値のセット(辞書のようなもの)を格納するデータ構造
var clients = make(map[*websocket.Conn]bool)
var broadcast = make(chan Message) // メッセージが来たら同期(chan)

type Message struct {
	Type    int
	Message []byte
}

func main() {
	// ginはGo言語用のWebフレームワーク！
	// 標準ライブラリのhttp.HandleFuncなどよりも高速！！
	r := gin.Default()

	// セッション管理の設定
	// cookie.NewStore()はCookieにセッションを保存するためのストア
	// 引数の[]byteは暗号化キー（本番では環境変数にするべき）
	store := cookie.NewStore([]byte("secret-key"))
	r.Use(sessions.Sessions("session", store))

	// 認証ルート（ログインしていなくてもアクセスできる）
	r.Any("/register", registerHandler)
	r.Any("/login", loginHandler)
	r.Any("/logout", logoutHandler)

	// websocketのupgraderを定義
	wsupgrader := websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
	}

	// authRequiredミドルウェアを適用したグループ
	// このグループ内のルートはログイン済みでないとアクセスできない
	authorized := r.Group("/")
	authorized.Use(authRequired)
	{
		// チャットページ
		authorized.GET("/chat", func(c *gin.Context) {
			http.ServeFile(c.Writer, c.Request, "templates/chat.html")
		})

		// websocket接続開始ルート
		authorized.GET("/ws", func(c *gin.Context) {
			// upgraderを呼び出すことで通常のhttp通信からwebsocketへupgrade
			// HTTPからWebSocketに切り替える場所
			conn, err := wsupgrader.Upgrade(c.Writer, c.Request, nil)
			// UpgradeはHTTPをWebSocketに変換する関数
			if err != nil {
				log.Printf("Failed to set websocket upgrade: %+v\n", err)
				return
			}

			// コネクションをclientsマップへ追加
			clients[conn] = true

			// 過去メッセージを新しい接続に送信する
			var chatHistory []ChatMessage
			db.Order("created_at asc").Find(&chatHistory)
			for _, m := range chatHistory {
				conn.WriteMessage(websocket.TextMessage, []byte(m.Content))
			}

			// 無限ループさせることでクライアントからのメッセージを受け付けられる状態にする
			// クライアントとのコネクションが切れた場合はReadMessage()関数からエラーが返る
			for {
				t, msg, err := conn.ReadMessage()
				if err != nil {
					log.Printf("ReadMessage Error. ERROR: %+v\n", err)
					break
				}
				// 受け取ったメッセージをbroadcastを通じてhandleMessage()関数へ渡す
				broadcast <- Message{Type: t, Message: msg} // Message型に合わせて値を代入
			}
		})
	}

	// 非同期でhandleMessageを実行
	go handleMessage()
	// handleMessageのみだと、その関数が終わるまで次には行かない
	// goroutineを使うことでhandleMessageを裏で動かすことができる
	// 並列処理が可能になる

	fmt.Println("サーバー起動: http://localhost:4001/register")
	r.Run(":4001")
}

// broadcastにメッセージがあれば、clientsに格納されているすべてのコネクションへ送信する
func handleMessage() {
	for {
		// broadcastからmessageにデータを送信
		message := <-broadcast
		// データベースに保存
		db.Create(&ChatMessage{Content: string(message.Message)})
		for client := range clients {
			err := client.WriteMessage(message.Type, message.Message)
			if err != nil {
				log.Printf("error: %v", err)
				client.Close()
				delete(clients, client)
			}
		}
	}
}
