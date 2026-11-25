package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
)

const (
	// pongWait: この時間だけ相手からの応答(pong)が無ければ「接続が死んでいる」とみなす
	pongWait = 60 * time.Second
	// pingPeriod: pongWaitより少し短い間隔でこちらからping送信する
	pingPeriod = (pongWait * 9) / 10
	// writeWait: 書き込み1回あたりのタイムアウト
	writeWait = 10 * time.Second
	// chatMinInterval: 1接続からのチャット送信を受け付ける最短間隔（連投・スパム対策）
	chatMinInterval = 500 * time.Millisecond
	// maxMessageLen: 1メッセージの最大文字数（chat.html の MAX_MESSAGE_LEN と同じ値にする）
	maxMessageLen = 2000
	// maxFrameBytes: 1回に受け取るデータの最大サイズ。これを超えるとライブラリが接続を切る。
	// 2000文字（日本語は1文字最大4バイト）と JSON の改行のエスケープなどを考えても十分な大きさにしている。
	// 上限がないと、巨大なデータを送りつけてサーバーのメモリを使い切らせる攻撃ができてしまう
	maxFrameBytes = 32 * 1024
	// historyPageSize: 履歴を1回に送る件数（入室したとき、上にスクロールしたとき）
	historyPageSize = 50
)

// Hub は「1つのチャットルーム」を表す。
// このHubの中身（clientsマップ）はrun()を動かしているgoroutine以外は
// 直接触らない。他のgoroutineはすべてチャンネル経由でHubに話しかける。
// → mapを直接ロックする必要がなくなる（Goの「メモリ共有より通信」の考え方）
type Hub struct {
	id uint

	clients map[*Client]bool

	register    chan *Client
	unregister  chan *Client
	broadcast   chan BroadcastMsg
	deleteReq   chan DeleteReq
	typingReq   chan TypingReq
	countReq    chan chan int
	roomDeleted chan struct{}
	direct      chan DirectMsg
}

// DirectMsg は、部屋の全員ではなく特定の1接続だけに送るメッセージ（エラーの通知など）。
// readPump から c.send に直接書き込むと、Hub が c.send を close した直後に書き込んで
// panic することがあるので、他のイベントと同じく Hub の goroutine を通して送る
type DirectMsg struct {
	Client *Client
	Msg    []byte
}

// DeleteReq はメッセージ削除リクエストをHubに伝えるためのデータ
type DeleteReq struct {
	MsgID  uint
	UserID uint
}

// TypingReq はタイピング状態の変化をHubに伝えるためのデータ
type TypingReq struct {
	Kind     string // "typing" または "stopTyping"
	UserID   uint
	Username string
}

// Client は「1つのWebSocket接続（＝1人のユーザーの1つのタブ）」を表す。
type Client struct {
	hub      *Hub
	conn     *websocket.Conn
	send     chan []byte // このクライアントへ送るべきメッセージのキュー
	userID   uint
	username string

	// lastChatAt: 直近でチャットを送信した時刻（レート制限用）。
	// readPumpのgoroutineだけが読み書きするので、他のフィールドと同様に
	// mutexなどの同期は不要（1接続につきreadPumpは1つしか動かないため）
	lastChatAt time.Time
}

// hubs は roomID → Hub の対応表（部屋のレジストリ）。
// hubsMu は「同時に2つのリクエストが同じ部屋のHubを新規作成しようとする」
// ようなケースからこのマップ自体を守るための唯一のミューテックス。
// （Hub内部のclientsマップとは保護対象が別なので、ここでしかロックを使わない）
var hubs = make(map[uint]*Hub)
var hubsMu sync.Mutex

// newHub はHubを作るだけで、run()はまだ動かさない
func newHub(roomID uint) *Hub {
	return &Hub{
		id:          roomID,
		clients:     make(map[*Client]bool),
		register:    make(chan *Client),
		unregister:  make(chan *Client),
		broadcast:   make(chan BroadcastMsg),
		deleteReq:   make(chan DeleteReq),
		typingReq:   make(chan TypingReq),
		countReq:    make(chan chan int),
		roomDeleted: make(chan struct{}),
		direct:      make(chan DirectMsg),
	}
}

// OnlineCount は現在の在室人数を安全に取得する。
// h.clientsはh.run()のgoroutineしか直接触らない、というルールを守るため、
// 「問い合わせ用のチャンネル」を送り、run()側に返信してもらう形にしている。
// （ここでmutexを使ってh.clientsを直接読むと、h.run()のgoroutineとの間で
//
//	同期されていない並行アクセスになり、更新が反映されずに古い値を読み続ける
//	ことがあるGoのメモリモデル上の問題を引き起こす）
func (h *Hub) OnlineCount() int {
	reply := make(chan int)
	h.countReq <- reply
	return <-reply
}

// NotifyRoomDeleted はこの部屋がDB上で削除されたことを、接続中の全クライアントに知らせる。
// h.clientsをHTTPハンドラ側から直接触らないように、こちらもchannel経由でHubの
// goroutineに処理してもらう（OnlineCountと同じ考え方）
func (h *Hub) NotifyRoomDeleted() {
	h.roomDeleted <- struct{}{}
}

// getOrCreateHub は指定した部屋のHubを返す。まだ存在しなければ作って
// 専用のgoroutine（h.run()）を起動する。
// 一度作られたHubはプロセスが動いている間ずっと生き続ける
// （空室になっても破棄しない。誰もいなくてもselectで待つだけなのでコストはほぼゼロ。
//
//	最後の1人の退室と次の人の入室が同時に起きた場合の競合を避けられる、
//	という理由でこの単純な方針を選んでいる）
func getOrCreateHub(roomID uint) *Hub {
	hubsMu.Lock()
	defer hubsMu.Unlock()

	if h, ok := hubs[roomID]; ok {
		return h
	}

	h := newHub(roomID)
	hubs[roomID] = h
	go h.run()
	return h
}

// run はこの部屋専用のイベントループ。1本のgoroutineで動き続け、
// register/unregister/broadcast/deleteReq/typingReq を順番に1つずつ処理する。
// これにより「DBへの保存」と「全員への配信」の間で競合が起きない。
func (h *Hub) run() {
	for {
		select {
		case c := <-h.register:
			// 部屋に加える前に、最新の履歴を送る。
			// メッセージの DB 保存（handleChat）もこの goroutine で1つずつ行っているので、
			// ここで読んだ履歴には「今までに保存されたメッセージ」がすべて入っていて、
			// これから届くメッセージは、部屋に加えたあとの配信で届く。
			// そのため、取りこぼしも二重に届くことも起きない
			// （c.send はまだ空なので、詰まって送れないことはない）
			select {
			case c.send <- historyMessage("history", h.id, 0):
			default:
			}
			h.clients[c] = true
			h.broadcastUserCount()

		case c := <-h.unregister:
			if _, ok := h.clients[c]; ok {
				delete(h.clients, c)
				close(c.send)
				h.broadcastUserCount()
			}

		case bm := <-h.broadcast:
			h.handleChat(bm)

		case req := <-h.deleteReq:
			h.handleDelete(req)

		case req := <-h.typingReq:
			h.handleTyping(req)

		case reply := <-h.countReq:
			reply <- len(h.clients)

		case <-h.roomDeleted:
			msg, _ := json.Marshal(WSMessage{Type: "roomDeleted"})
			h.send(msg)

		case dm := <-h.direct:
			// まだ部屋にいる（c.send が close されていない）ときだけ送る。
			// 送信キューが詰まっていたら、通知は諦めて捨てる（エラーの通知のために全体を止めない）
			if _, ok := h.clients[dm.Client]; ok {
				select {
				case dm.Client.send <- dm.Msg:
				default:
				}
			}
		}
	}
}

// send はこのHubに参加している全クライアントへメッセージを送る。
// 送信先クライアントのsendチャンネルが詰まっている（bufferが満杯）場合は
// そのクライアントを切断扱いにする（遅いクライアントのために全体を止めない）
func (h *Hub) send(msg []byte) {
	for c := range h.clients {
		select {
		case c.send <- msg:
		default:
			close(c.send)
			delete(h.clients, c)
		}
	}
}

func (h *Hub) broadcastUserCount() {
	msg, _ := json.Marshal(WSMessage{Type: "userCount", Count: len(h.clients)})
	h.send(msg)
}

// formatWireTime はブラウザに送る時刻を、タイムゾーン付きの形式（RFC 3339。例：2026-11-24T16:03:42Z）にする。
// 表示用の文字（"2006/01/02 15:04:05" など）にここで変換すると、サーバーのタイムゾーン
// （本番のコンテナは UTC）のまま表示されてしまう。表示はブラウザ側（chat.html の formatTime）で、
// 見ている人の PC のタイムゾーンに合わせて行う
func formatWireTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// loadHistory は、部屋のメッセージを新しいほうから historyPageSize 件読み、古い順に並べて返す。
// beforeID が 0 なら最新から、0 以外ならその ID より古いものを読む（上にスクロールしたとき）。
// hasMore は、それよりさらに古いメッセージが残っているか
func loadHistory(roomID, beforeID uint) (msgs []WSMessage, hasMore bool) {
	query := db.Where("room_id = ?", roomID)
	if beforeID > 0 {
		query = query.Where("id < ?", beforeID)
	}

	// 1件多く読んで、読めたら「まだ古いものがある」とわかる（件数を数える問い合わせを別にしなくて済む）。
	// 並べ替えは created_at ではなく id で行う。id は保存した順に増えるので順番は同じで、
	// 「この ID より古いもの」という続きの読み方と、models.go の索引（room_id, id）をそのまま使える
	var rows []ChatMessage
	if err := query.Order("id desc").Limit(historyPageSize + 1).Find(&rows).Error; err != nil {
		log.Printf("履歴を読めませんでした: %v", err)
		return []WSMessage{}, false
	}

	if len(rows) > historyPageSize {
		hasMore = true
		rows = rows[:historyPageSize]
	}

	// 新しい順に読んだので、後ろから詰めて古い順にする（画面では上が古い）
	msgs = make([]WSMessage, len(rows))
	for i, m := range rows {
		msgs[len(rows)-1-i] = WSMessage{
			Type:      "chat",
			ID:        m.ID,
			Content:   m.Content,
			UserID:    m.UserID,
			Username:  m.Username,
			CreatedAt: formatWireTime(m.CreatedAt),
		}
	}
	return msgs, hasMore
}

// historyMessage は、履歴をまとめて1つのメッセージにする（1件ずつ送ると、ブラウザが1件ごとに描画して重くなる）。
// msgType は、入室したときが "history"、上にスクロールして古いものを読んだときが "olderHistory"
func historyMessage(msgType string, roomID, beforeID uint) []byte {
	msgs, hasMore := loadHistory(roomID, beforeID)
	b, _ := json.Marshal(WSMessage{Type: msgType, Messages: msgs, HasMore: hasMore})
	return b
}

// handleChat はチャットメッセージをDBに保存し、全員に配信する
// （旧 handleMessage 相当の処理をルーム単位に持ち込んだもの）
func (h *Hub) handleChat(bm BroadcastMsg) {
	chatMsg := ChatMessage{Content: bm.Content, UserID: bm.UserID, Username: bm.Username, RoomID: h.id}
	db.Create(&chatMsg)

	msg, _ := json.Marshal(WSMessage{
		Type:      "chat",
		ID:        chatMsg.ID,
		Content:   bm.Content,
		UserID:    bm.UserID,
		Username:  bm.Username,
		CreatedAt: formatWireTime(chatMsg.CreatedAt),
	})
	h.send(msg)
}

// handleDelete はメッセージの所有者確認・DB削除・全員への削除通知を行う
func (h *Hub) handleDelete(req DeleteReq) {
	var chatMsg ChatMessage
	db.First(&chatMsg, req.MsgID)

	if chatMsg.ID == 0 || chatMsg.UserID != req.UserID || chatMsg.RoomID != h.id {
		return
	}

	db.Delete(&chatMsg)

	msg, _ := json.Marshal(WSMessage{Type: "delete", ID: req.MsgID})
	h.send(msg)
}

// handleTyping はタイピング状態を全員に中継する（状態は保存しない）
func (h *Hub) handleTyping(req TypingReq) {
	msg, _ := json.Marshal(WSMessage{Type: req.Kind, UserID: req.UserID, Username: req.Username})
	h.send(msg)
}

// readPump はこの接続からの読み取りだけを行う専用goroutine。
// 読んだイベントはHubのチャンネルに渡すだけで、自分ではclientsマップに触らない。
//
// ブラウザがページ遷移やタブ閉じでWebSocketを綺麗に閉じてくれれば
// ReadMessageはすぐエラーを返すが、スリープやネットワーク切断などで
// 「相手が応答しなくなった」だけの場合は何もしなければ永遠にブロックし続けてしまう。
// それを防ぐため、一定時間(pongWait)応答が無ければ強制的にタイムアウトさせる。
func (c *Client) readPump() {
	defer func() {
		c.hub.unregister <- c
		c.conn.Close()
	}()

	c.conn.SetReadLimit(maxFrameBytes)
	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		_, rawMsg, err := c.conn.ReadMessage()
		if err != nil {
			break
		}

		var incoming WSMessage
		if err := json.Unmarshal(rawMsg, &incoming); err != nil {
			continue
		}

		switch incoming.Type {
		case "chat":
			// ブラウザ側でも同じ確認をしているが、ブラウザの確認は開発者ツールで簡単に外せるので、
			// サーバーでも必ず確認する。受け付けないときは、DBへの保存も全員への配信もしない

			// 空白や改行だけのメッセージは、何も言わずに無視する
			if strings.TrimSpace(incoming.Content) == "" {
				continue
			}
			// 文字数は utf8.RuneCountInString で数える（len だと日本語1文字を3と数えてしまう）
			if utf8.RuneCountInString(incoming.Content) > maxMessageLen {
				c.notify(fmt.Sprintf("メッセージは%d文字以内にしてください", maxMessageLen))
				continue
			}
			// 連投スパム対策: 前回の送信からchatMinIntervalが経っていなければ受け付けない
			if time.Since(c.lastChatAt) < chatMinInterval {
				c.notify("送信が速すぎます。少し待ってから送ってください")
				continue
			}
			c.lastChatAt = time.Now()
			c.hub.broadcast <- BroadcastMsg{Content: incoming.Content, UserID: c.userID, Username: c.username}
		case "delete":
			c.hub.deleteReq <- DeleteReq{MsgID: incoming.ID, UserID: c.userID}
		case "typing", "stopTyping":
			c.hub.typingReq <- TypingReq{Kind: incoming.Type, UserID: c.userID, Username: c.username}
		case "loadOlder":
			// 上にスクロールしたときの「もっと古い履歴をください」。
			// 古いメッセージは配信と重ならないので、Hub の goroutine ではなくここで DB から読み、
			// 部屋全体を待たせないようにする。送るのは notify と同じく Hub に任せる
			if incoming.Before == 0 {
				continue
			}
			msg := historyMessage("olderHistory", c.hub.id, incoming.Before)
			c.hub.direct <- DirectMsg{Client: c, Msg: msg}
		}
	}
}

// notify は、この接続の本人にだけエラーなどの通知を送る（type: "error"）。
// 送るのは Hub の goroutine に任せる（DirectMsg の説明を参照）
func (c *Client) notify(text string) {
	msg, _ := json.Marshal(WSMessage{Type: "error", Content: text})
	c.hub.direct <- DirectMsg{Client: c, Msg: msg}
}

// writePump はこの接続への書き込みだけを行う専用goroutine。
// gorilla/websocketは同じコネクションへの同時書き込みを許さないため、
// 「書き込みを行うのは必ずこのgoroutineだけ」というルールにして競合を防ぐ。
//
// 定期的にping（pingPeriodおき）を送ることで、readPump側のReadDeadlineを
// 相手が生きている限り延長し続けてもらう。相手が応答しなくなればpongが
// 返らなくなり、readPump側がpongWait経過後にタイムアウトしてunregisterされる。
func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case msg, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				// c.sendがcloseされた（Hubがunregister処理した）
				// クライアントに正常クローズを伝えて終了する
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				log.Printf("write error: %v", err)
				return
			}

		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
