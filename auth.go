package main

import (
	"net/http"

	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

// registerHandlerはユーザー登録を処理するハンドラー
func registerHandler(c *gin.Context) {
	switch c.Request.Method {
	case http.MethodGet:
		// GETのときは登録フォームのHTMLを返す
		c.File("templates/register.html")
	case http.MethodPost:
		username := c.PostForm("username")
		password := c.PostForm("password")

		if username == "" || password == "" {
			c.String(http.StatusBadRequest, "ユーザー名とパスワードを入力してください")
			return
		}

		// パスワードをハッシュ化して保存（平文で保存しない！）
		hashed, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			c.String(http.StatusInternalServerError, "Internal Server Error")
			return
		}

		result := db.Create(&User{Username: username, Password: string(hashed)})
		if result.Error != nil {
			c.String(http.StatusBadRequest, "このユーザー名は既に使用されています")
			return
		}

		// db.Create(&User{Username: username, Password: string(hashed)})
		// 登録後はログインページへリダイレクト
		c.Redirect(http.StatusFound, "/login")
	}
}

// loginHandlerはログインを処理するハンドラー
func loginHandler(c *gin.Context) {
	switch c.Request.Method {
	case http.MethodGet:
		c.File("templates/login.html")
	case http.MethodPost:
		username := c.PostForm("username")
		password := c.PostForm("password")

		// DBからユーザーを検索
		var user User
		db.Where("username = ?", username).First(&user)

		if user.ID == 0 { // ユーザーが存在しない場合
			c.String(http.StatusNotFound, "User not found")
			return
		}

		// 入力パスワードとハッシュ化済みパスワードを比較
		err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(password))
		if err != nil {
			c.String(http.StatusUnauthorized, "Invalid password")
			return
		}

		// ★ セッションにuserIDを保存！これがログイン状態の正体
		// セッションはCookieに暗号化されてブラウザに保存される
		session := sessions.Default(c)
		session.Set("userID", user.ID)
		session.Save()

		c.Redirect(http.StatusFound, "/chat")
	}
}

// logoutHandlerはログアウトを処理するハンドラー
func logoutHandler(c *gin.Context) {
	switch c.Request.Method {
	case http.MethodGet:
		c.File("templates/logout.html")
	case http.MethodPost:
		// セッションを削除することでログアウト
		session := sessions.Default(c)
		// MaxAge: -1 でブラウザにCookieを今すぐ削除するよう指示する
		// Clear()だけだと値が空になるだけでCookie自体は残ってしまう
		session.Options(sessions.Options{MaxAge: -1})
		session.Clear()
		session.Save()

		c.Redirect(http.StatusFound, "/login")
	}
}

// authRequiredはログイン済みかどうかチェックするミドルウェア
// /chat や /ws へのアクセス前にこの関数が呼ばれる
func authRequired(c *gin.Context) {
	session := sessions.Default(c)
	userID := session.Get("userID")

	if userID == nil {
		// セッションにuserIDがなければ未ログイン → ログインページへ
		c.Redirect(http.StatusFound, "/login")
		c.Abort() // 以降のハンドラーを実行しない
		return
	}

	c.Next() // 認証OK → 次の処理へ進む
}
