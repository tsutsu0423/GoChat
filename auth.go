package main

import (
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

// 登録時の入力ルール（register.html の minlength / maxlength と同じ値にする）
const (
	usernameMinLen = 3
	usernameMaxLen = 20
	passwordMinLen = 8
	// bcrypt は72バイトまでしか扱えず、それより長いとハッシュ化がエラーになる
	passwordMaxBytes = 72
)

// dummyHash は、存在しないユーザー名でログインされたときに比較するためのハッシュ。
// ユーザーがいないときに bcrypt の比較を飛ばすと、そのぶん返事が速くなり、
// 応答時間の差から「その名前は登録済みかどうか」を推測されてしまう。
// いないときもダミーと比較して、時間をそろえる。
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("dummy-password-for-timing"), bcrypt.DefaultCost)

// isLoggedIn はセッションにユーザーIDが入っているか（ログイン済みか）を返す
func isLoggedIn(c *gin.Context) bool {
	return sessions.Default(c).Get("userID") != nil
}

// startSession はセッションにユーザーIDを保存して、ログイン状態にする。
// セッションは署名付きの Cookie としてブラウザに保存される
func startSession(c *gin.Context, userID uint) error {
	session := sessions.Default(c)
	session.Set("userID", userID)
	return session.Save()
}

// registerHandler はユーザー登録を処理するハンドラー
func registerHandler(c *gin.Context) {
	switch c.Request.Method {
	case http.MethodGet:
		// すでにログインしていれば、登録画面ではなく一覧へ
		if isLoggedIn(c) {
			c.Redirect(http.StatusFound, "/")
			return
		}
		c.HTML(http.StatusOK, "register.html", gin.H{})

	case http.MethodPost:
		// 前後の空白は取り除く（"  taro " と "taro" を別のユーザーにしないため）
		username := strings.TrimSpace(c.PostForm("username"))
		password := c.PostForm("password")
		passwordConfirm := c.PostForm("password_confirm")

		// エラーのときは登録画面をもう一度表示し、入力したユーザー名を残す
		fail := func(status int, message string) {
			c.HTML(status, "register.html", gin.H{"Error": message, "Username": username})
		}

		// 文字数は utf8.RuneCountInString で数える（len だと日本語1文字を3と数えてしまう）
		usernameLen := utf8.RuneCountInString(username)

		switch {
		case username == "" || password == "":
			fail(http.StatusBadRequest, "ユーザー名とパスワードを入力してください")
			return
		case usernameLen < usernameMinLen || usernameLen > usernameMaxLen:
			fail(http.StatusBadRequest, "ユーザー名は3〜20文字で入力してください")
			return
		case utf8.RuneCountInString(password) < passwordMinLen:
			fail(http.StatusBadRequest, "パスワードは8文字以上で入力してください")
			return
		case len(password) > passwordMaxBytes:
			fail(http.StatusBadRequest, "パスワードが長すぎます（半角72文字まで）")
			return
		case password != passwordConfirm:
			fail(http.StatusBadRequest, "パスワードと確認用のパスワードが一致しません")
			return
		}

		// パスワードをハッシュ化して保存（平文で保存しない！）
		hashed, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			fail(http.StatusInternalServerError, "登録に失敗しました。時間をおいてもう一度お試しください")
			return
		}

		user := User{Username: username, Password: string(hashed)}
		if err := db.Create(&user).Error; err != nil {
			// Username には unique 制約があるので、重複するとここでエラーになる
			fail(http.StatusConflict, "このユーザー名は既に使用されています")
			return
		}

		// 登録できたら、そのままログイン状態にして一覧へ（もう一度ログインさせない）
		// 作ったユーザーのIDは、db.Create のあとの user.ID に入っている
		if err := startSession(c, user.ID); err != nil {
			c.Redirect(http.StatusFound, "/login")
			return
		}
		c.Redirect(http.StatusFound, "/")
	}
}

// loginHandler はログインを処理するハンドラー
func loginHandler(c *gin.Context) {
	switch c.Request.Method {
	case http.MethodGet:
		// すでにログインしていれば、ログイン画面ではなく一覧へ
		if isLoggedIn(c) {
			c.Redirect(http.StatusFound, "/")
			return
		}
		c.HTML(http.StatusOK, "login.html", gin.H{})

	case http.MethodPost:
		username := strings.TrimSpace(c.PostForm("username"))
		password := c.PostForm("password")

		if username == "" || password == "" {
			c.HTML(http.StatusBadRequest, "login.html", gin.H{
				"Error":    "ユーザー名とパスワードを入力してください",
				"Username": username,
			})
			return
		}

		// DBからユーザーを検索
		var user User
		db.Where("username = ?", username).First(&user)

		// ユーザーがいないときも、ダミーのハッシュと比較して時間をそろえる（dummyHash の説明を参照）
		hash := dummyHash
		if user.ID != 0 {
			hash = []byte(user.Password)
		}
		err := bcrypt.CompareHashAndPassword(hash, []byte(password))

		// 「ユーザーがいない」と「パスワード違い」を同じ文にする。
		// 分けると、ある名前が登録済みかどうかを他人が調べられてしまう
		if user.ID == 0 || err != nil {
			c.HTML(http.StatusUnauthorized, "login.html", gin.H{
				"Error":    "ユーザー名またはパスワードが違います",
				"Username": username,
			})
			return
		}

		if err := startSession(c, user.ID); err != nil {
			c.HTML(http.StatusInternalServerError, "login.html", gin.H{
				"Error":    "ログインに失敗しました。時間をおいてもう一度お試しください",
				"Username": username,
			})
			return
		}
		c.Redirect(http.StatusFound, "/")
	}
}

// logoutHandler はログアウトを処理するハンドラー（POST のみ）。
// GET でログアウトできると、他のサイトに <img src=".../logout"> と書かれただけで
// 勝手にログアウトさせられてしまうので、状態を変える操作は POST にする
func logoutHandler(c *gin.Context) {
	session := sessions.Default(c)
	// MaxAge: -1 でブラウザにCookieを今すぐ削除するよう指示する
	// Clear()だけだと値が空になるだけでCookie自体は残ってしまう
	session.Options(sessions.Options{
		Path:     "/",
		MaxAge:   -1,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	session.Clear()
	session.Save()

	c.Redirect(http.StatusFound, "/login")
}

// authRequiredはログイン済みかどうかチェックするミドルウェア
// /chat や /ws へのアクセス前にこの関数が呼ばれる
func authRequired(c *gin.Context) {
	if !isLoggedIn(c) {
		// セッションにuserIDがなければ未ログイン → ログインページへ
		c.Redirect(http.StatusFound, "/login")
		c.Abort() // 以降のハンドラーを実行しない
		return
	}

	c.Next() // 認証OK → 次の処理へ進む
}
