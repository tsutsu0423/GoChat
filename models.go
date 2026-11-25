package main

import (
	"log"
	"os"

	"github.com/joho/godotenv"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

var db *gorm.DB

// Userはユーザー情報を保持するstructで、DBのテーブルに対応している
type User struct {
	gorm.Model            // ID, CreatedAt, UpdatedAt, DeletedAtを自動で追加
	Username string `gorm:"unique"` // unique制約でユーザー名の重複を防ぐ
	Password string
}

// Roomはチャットグループ（部屋）を保持するstruct
type Room struct {
	gorm.Model
	Name      string `gorm:"unique"` // 部屋名（重複禁止）
	CreatedBy uint   // 作成したユーザーのID
}

// ChatMessageはチャットのメッセージを保持するstruct
type ChatMessage struct {
	gorm.Model
	Content  string
	UserID   uint   // 誰が送ったか（Userのidを保存する）
	Username string // 誰が送ったか（表示用のユーザー名）
	RoomID   uint   // どの部屋のメッセージか
}

func init() {
	godotenv.Load() // .envファイルを読み込む

	dsn := os.Getenv("DSN") // .envに書いたDB接続情報を取得
	var err error
	db, err = gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatal(err)
	}

	// structの定義に合わせてDBテーブルを自動作成・更新
	db.AutoMigrate(&User{}, &Room{}, &ChatMessage{})

	// 履歴は「この部屋の、ID が○○より小さいメッセージを新しい順に50件」という形で読む（hub.go の loadHistory）。
	// その検索を速くするため、「部屋ID＋ID」の順に並べた索引（インデックス）を作る。
	// 索引がないと、メッセージが増えるほど表の全行を調べることになり遅くなる。
	// IF NOT EXISTS なので、2回目以降の起動では何もしない
	if err := db.Exec("CREATE INDEX IF NOT EXISTS idx_chat_messages_room_id_id ON chat_messages (room_id, id)").Error; err != nil {
		log.Printf("インデックスを作れませんでした: %v", err)
	}
}
