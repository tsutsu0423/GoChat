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

// ChatMessageはチャットのメッセージを保持するstruct
type ChatMessage struct {
	gorm.Model
	Content  string
	UserID   uint   // 誰が送ったか（Userのidを保存する）
	Username string // 誰が送ったか（表示用のユーザー名）
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
	db.AutoMigrate(&User{}, &ChatMessage{})
}
