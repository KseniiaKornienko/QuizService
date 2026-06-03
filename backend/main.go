package main

import (
	"database/sql"
	"log"
	"net/http"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/gorilla/sessions"
)

var db *sql.DB
var store = sessions.NewCookieStore([]byte("my-secret"))

func main() {
	var err error
	db, err = sql.Open("mysql", "root:my-secret@tcp(127.0.0.1:3307)/quizdb?parseTime=true&loc=Local")
	if err != nil {
		log.Fatal("Ошибка подключения к БД:", err)
	}
	if err = db.Ping(); err != nil {
		log.Fatal("БД недоступна:", err)
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(25)
	db.SetConnMaxIdleTime(2 * time.Minute)
	db.SetConnMaxLifetime(10 * time.Minute)
	http.HandleFunc("/", handleHomePage)
	http.HandleFunc("/register", handleRegister)
	http.HandleFunc("/login", handleLogin)
	http.HandleFunc("/logout", handleLogout)
	http.HandleFunc("/quiz/", handleViewQuiz)
	http.HandleFunc("/my-quizzes/", handleMyQuizzes)
	http.HandleFunc("/edit-quiz", handleEditQuiz)
	http.HandleFunc("/edit-quiz/", handleEditQuiz)
	http.HandleFunc("/delete-quiz/", handleDeleteQuiz)
	http.HandleFunc("/question-bank/", handleQuestionsBank)
	http.HandleFunc("/history/", handleHistory)
	http.HandleFunc("/quiz-stats/", handleQuizStats)
	http.HandleFunc("/profile/change-username", handleChangeUsername)
	http.HandleFunc("/profile/change-password", handleChangePassword)
	http.HandleFunc("/api/upload-option-image", handleUploadOptionImage)
	http.HandleFunc("/api/upload-question-media", handleUploadQuestionMedia)
	http.HandleFunc("/api/quiz-private-key/validate", handleValidateQuizPrivateKey)
	http.HandleFunc("/api/quiz-status/", handleQuizStatusToggle)
	http.Handle("/uploads/", http.StripPrefix("/uploads/", http.FileServer(http.Dir("./uploads"))))

	server := &http.Server{
		Addr:              ":5050",
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Fatal(server.ListenAndServe())
}
