package main

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var usernamePattern = regexp.MustCompile(`^[a-zA-Z0-9_]{3,}$`)

func nullStringToString(ns sql.NullString) string {
	if ns.Valid {
		return ns.String
	}
	return ""
}

func detectQuestionMediaType(rawType, rawURL string) string {
	t := strings.ToLower(strings.TrimSpace(rawType))
	switch t {
	case "image", "photo":
		return "image"
	case "video":
		return "video"
	}

	cleanURL := strings.TrimSpace(rawURL)
	if cleanURL == "" {
		return ""
	}
	if idx := strings.IndexAny(cleanURL, "?#"); idx >= 0 {
		cleanURL = cleanURL[:idx]
	}
	ext := strings.ToLower(filepath.Ext(cleanURL))
	switch ext {
	case ".mp4", ".webm", ".ogg", ".mov", ".m4v":
		return "video"
	case ".jpg", ".jpeg", ".png", ".webp", ".gif", ".bmp", ".svg":
		return "image"
	default:
		return "image"
	}
}

func normalizeQuestionMedia(rawURL, rawType string) (string, string) {
	url := strings.TrimSpace(rawURL)
	if url == "" {
		return "", ""
	}
	return url, detectQuestionMediaType(rawType, url)
}

func isSingleChoiceQuestionType(t string) bool {
	t = strings.ToLower(strings.TrimSpace(t))
	return t == "single" || t == "photo_single"
}

func isMultipleChoiceQuestionType(t string) bool {
	t = strings.ToLower(strings.TrimSpace(t))
	return t == "multiple" || t == "photo_multiple"
}

func isChoiceQuestionType(t string) bool {
	return isSingleChoiceQuestionType(t) || isMultipleChoiceQuestionType(t)
}

func isOpenQuestionType(t string) bool {
	t = strings.ToLower(strings.TrimSpace(t))
	return t == "open"
}

func normalizeOpenAnswer(s string) string {
	s = strings.TrimSpace(strings.ToLower(strings.ReplaceAll(s, "ё", "е")))
	if s == "" {
		return ""
	}
	return strings.Join(strings.Fields(s), " ")
}

func uniqueSortedInts(ids []int) []int {
	m := make(map[int]struct{}, len(ids))
	for _, id := range ids {
		if id > 0 {
			m[id] = struct{}{}
		}
	}
	out := make([]int, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	sort.Ints(out)
	return out
}

func sameIntSet(a, b []int) bool {
	aa := uniqueSortedInts(a)
	bb := uniqueSortedInts(b)
	if len(aa) != len(bb) {
		return false
	}
	for i := range aa {
		if aa[i] != bb[i] {
			return false
		}
	}
	return true
}

func makeIntSet(ids []int) map[int]struct{} {
	m := make(map[int]struct{}, len(ids))
	for _, id := range ids {
		if id > 0 {
			m[id] = struct{}{}
		}
	}
	return m
}

func wantsJSON(r *http.Request) bool {
	if strings.EqualFold(r.Header.Get("X-Requested-With"), "XMLHttpRequest") {
		return true
	}
	accept := r.Header.Get("Accept")
	return strings.Contains(accept, "application/json")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func formatDurationHuman(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	secs := int(d.Seconds())
	h := secs / 3600
	m := (secs % 3600) / 60
	s := secs % 60
	if h > 0 {
		return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}

func sanitizeParticipantName(raw string) string {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "Участник"
	}
	if len([]rune(name)) > 64 {
		r := []rune(name)
		name = string(r[:64])
	}
	return name
}

func sanitizeTeamName(raw string) string {
	name := strings.TrimSpace(raw)
	if len([]rune(name)) > 64 {
		r := []rune(name)
		name = string(r[:64])
	}
	return name
}

func normalizeParticipationMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "team_manual", "team", "manual_team":
		return "team_manual"
	case "team_roulette", "roulette", "random_teams", "team_random", "team_auto", "random":
		return "team_roulette"
	default:
		return "individual"
	}
}

func normalizeTeamCount(mode string, count int) int {
	mode = normalizeParticipationMode(mode)
	if mode != "team_roulette" {
		return 0
	}
	if count < 2 {
		count = 2
	}
	if count > 100 {
		count = 100
	}
	return count
}

func isTeamMode(mode string) bool {
	mode = normalizeParticipationMode(mode)
	return mode == "team_manual" || mode == "team_roulette"
}

func isTeamRouletteMode(mode string) bool {
	return normalizeParticipationMode(mode) == "team_roulette"
}

func participationModeLabel(mode string) string {
	switch normalizeParticipationMode(mode) {
	case "team_manual", "team_roulette":
		return "Командный"
	default:
		return "Индивидуальный"
	}
}

func rouletteTeamName(n int) string {
	if n < 1 {
		n = 1
	}
	return strconv.Itoa(n)
}

var legacyRouletteTeamNamePattern = regexp.MustCompile(`(?i)^команда\s+(\d+)$`)

func normalizeRouletteTeamName(raw string) (int, string, bool) {
	trimmed := sanitizeTeamName(raw)
	if trimmed == "" {
		return 0, "", false
	}
	if n, err := strconv.Atoi(trimmed); err == nil && n > 0 {
		return n, rouletteTeamName(n), true
	}
	m := legacyRouletteTeamNamePattern.FindStringSubmatch(trimmed)
	if len(m) == 2 {
		n, err := strconv.Atoi(m[1])
		if err == nil && n > 0 {
			return n, rouletteTeamName(n), true
		}
	}
	return 0, trimmed, false
}

func randomTeamNumber(teamCount int) int {
	if teamCount <= 1 {
		return 1
	}
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err == nil {
		v := int(buf[0])<<24 | int(buf[1])<<16 | int(buf[2])<<8 | int(buf[3])
		if v < 0 {
			v = -v
		}
		return (v % teamCount) + 1
	}
	v := int(time.Now().UnixNano())
	if v < 0 {
		v = -v
	}
	return (v % teamCount) + 1
}

func normalizeParticipantIdentityName(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

func participantIdentityKey(userID int, participantName string) string {
	if name := normalizeParticipantIdentityName(participantName); name != "" {
		return "name:" + name
	}
	if userID > 0 {
		return fmt.Sprintf("uid:%d", userID)
	}
	return ""
}

func rouletteTeamFromStored(teamNumber sql.NullInt64, teamName sql.NullString, teamCount int) (int, string, bool) {
	if teamNumber.Valid {
		n := int(teamNumber.Int64)
		if n >= 1 && n <= teamCount {
			return n, rouletteTeamName(n), true
		}
	}
	if teamName.Valid {
		if n, normalized, ok := normalizeRouletteTeamName(teamName.String); ok && n >= 1 && n <= teamCount {
			return n, normalized, true
		}
	}
	return 0, "", false
}

func latestRouletteTeamForParticipant(quizID int, participantName string, userID int, teamCount int) (int, string) {
	if teamCount <= 1 {
		n := randomTeamNumber(teamCount)
		return n, rouletteTeamName(n)
	}

	lookup := func(where string, args ...any) (int, string, bool) {
		var teamNumber sql.NullInt64
		var teamName sql.NullString
		query := `SELECT team_number, team_name
			FROM quiz_sessions
			WHERE quiz_id = ? AND ` + where + ` AND (
				team_number IS NOT NULL OR COALESCE(NULLIF(TRIM(team_name), ''), '') <> ''
			)
			ORDER BY finished_at DESC, id DESC
			LIMIT 1`
		queryArgs := append([]any{quizID}, args...)
		err := db.QueryRow(query, queryArgs...).Scan(&teamNumber, &teamName)
		if err == nil {
			return rouletteTeamFromStored(teamNumber, teamName, teamCount)
		}
		return 0, "", false
	}

	if name := normalizeParticipantIdentityName(participantName); name != "" {
		if n, teamName, ok := lookup("LOWER(TRIM(participant_name)) = ?", name); ok {
			return n, teamName
		}
	} else if userID > 0 {
		if n, teamName, ok := lookup("user_id = ?", userID); ok {
			return n, teamName
		}
	}

	n := randomTeamNumber(teamCount)
	return n, rouletteTeamName(n)
}

func buildQuizTeamLeaderboard(quizID int, currentTeam string, rouletteMode bool) ([]TeamStanding, int, error) {
	rows, err := db.Query(`SELECT user_id,
            COALESCE(NULLIF(TRIM(team_name), ''), '') AS team_name,
            team_number,
            score,
            COALESCE(NULLIF(TRIM(participant_name), ''), '') AS participant_name
        FROM quiz_sessions
        WHERE quiz_id = ? AND (
            team_number IS NOT NULL OR COALESCE(NULLIF(TRIM(team_name), ''), '') <> ''
        )`, quizID)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	type participantAgg struct {
		bestScore int
	}
	type teamAgg struct {
		displayName  string
		participants map[string]*participantAgg
		anonScore    int
		anonCount    int
	}
	teams := make(map[string]*teamAgg)

	for rows.Next() {
		var userIDValue sql.NullInt64
		var rawTeamName string
		var teamNumber sql.NullInt64
		var score int
		var participantName string
		if err := rows.Scan(&userIDValue, &rawTeamName, &teamNumber, &score, &participantName); err != nil {
			return nil, 0, err
		}
		userID := 0
		if userIDValue.Valid {
			userID = int(userIDValue.Int64)
		}

		teamName := ""
		if teamNumber.Valid && teamNumber.Int64 > 0 {
			teamName = rouletteTeamName(int(teamNumber.Int64))
		} else if rouletteMode {
			if _, normalized, ok := normalizeRouletteTeamName(rawTeamName); ok {
				teamName = normalized
			}
		} else {
			teamName = sanitizeTeamName(rawTeamName)
		}
		if teamName == "" {
			continue
		}

		teamKey := strings.ToLower(teamName)
		team := teams[teamKey]
		if team == nil {
			team = &teamAgg{
				displayName:  teamName,
				participants: make(map[string]*participantAgg),
			}
			teams[teamKey] = team
		}

		participantKey := participantIdentityKey(userID, participantName)
		if participantKey == "" {
			team.anonScore += score
			team.anonCount++
			continue
		}

		member := team.participants[participantKey]
		if member == nil {
			team.participants[participantKey] = &participantAgg{bestScore: score}
			continue
		}
		if score > member.bestScore {
			member.bestScore = score
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	out := make([]TeamStanding, 0, len(teams))
	for _, team := range teams {
		totalScore := team.anonScore
		totalMembers := team.anonCount
		for _, member := range team.participants {
			totalScore += member.bestScore
			totalMembers++
		}
		out = append(out, TeamStanding{
			TeamName: team.displayName,
			Score:    totalScore,
			Members:  totalMembers,
		})
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if out[i].Members != out[j].Members {
			return out[i].Members > out[j].Members
		}
		return strings.ToLower(out[i].TeamName) < strings.ToLower(out[j].TeamName)
	})

	currentRank := 0
	for i := range out {
		out[i].Rank = i + 1
		out[i].IsCurrent = currentTeam != "" && strings.EqualFold(out[i].TeamName, currentTeam)
		if out[i].IsCurrent {
			currentRank = out[i].Rank
		}
	}
	return out, currentRank, nil
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	const hexdigits = "0123456789abcdef"
	out := make([]byte, n*2)
	for i := 0; i < n; i++ {
		out[i*2] = hexdigits[b[i]>>4]
		out[i*2+1] = hexdigits[b[i]&0x0f]
	}
	return string(out)
}

func handleUploadOptionImage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	if !isAuthenticated(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "unauthorized"})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 6<<20)
	if err := r.ParseMultipartForm(6 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "bad multipart"})
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "file missing"})
		return
	}
	defer file.Close()

	ext := strings.ToLower(filepath.Ext(header.Filename))
	if ext == ".jpeg" {
		ext = ".jpg"
	}
	switch ext {
	case ".jpg", ".png", ".webp", ".gif":
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "unsupported file type"})
		return
	}

	if err := os.MkdirAll("uploads/options", 0o755); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "cannot create dir"})
		return
	}

	name := randHex(16) + ext
	dstPath := filepath.Join("uploads/options", name)
	dst, err := os.Create(dstPath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "cannot save"})
		return
	}
	defer dst.Close()
	if _, err := io.Copy(dst, file); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "cannot write"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "url": "/uploads/options/" + name})
}

func handleUploadQuestionMedia(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	if !isAuthenticated(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "unauthorized"})
		return
	}

	const maxUploadSize = 50 << 20
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	if err := r.ParseMultipartForm(maxUploadSize); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "bad multipart"})
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "file missing"})
		return
	}
	defer file.Close()

	ext := strings.ToLower(filepath.Ext(header.Filename))
	if ext == ".jpeg" {
		ext = ".jpg"
	}

	mediaType := ""
	switch ext {
	case ".jpg", ".png", ".webp", ".gif":
		mediaType = "image"
	case ".mp4", ".webm", ".ogg", ".mov", ".m4v":
		mediaType = "video"
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "unsupported file type"})
		return
	}

	if err := os.MkdirAll("uploads/question-media", 0o755); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "cannot create dir"})
		return
	}

	name := randHex(16) + ext
	dstPath := filepath.Join("uploads/question-media", name)
	dst, err := os.Create(dstPath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "cannot save"})
		return
	}
	defer dst.Close()
	if _, err := io.Copy(dst, file); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "cannot write"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "url": "/uploads/question-media/" + name, "media_type": mediaType})
}

func getUserID(r *http.Request) (int, bool) {
	session, _ := store.Get(r, "session-name")
	if userID, ok := session.Values["userID"].(int); ok {
		return userID, true
	}
	return 0, false
}

func getCurrentUsername(r *http.Request) (string, bool) {
	session, _ := store.Get(r, "session-name")
	if username, ok := session.Values["username"].(string); ok {
		return username, true
	}
	return "", false
}

func isAuthenticated(r *http.Request) bool {
	session, _ := store.Get(r, "session-name")
	if auth, ok := session.Values["authenticated"].(bool); ok && auth {
		return true
	}
	return false
}

const quizSelectColumns = "id, title, description, category, author, private_key, privacy, status, time_limit_seconds, pass_score, participation_mode, team_count"

func loadQuizQuestionsWithOptions(quizID int) ([]QuestionWithOptions, error) {
	rows, err := db.Query("SELECT id, text, type, points, author_id, quiz_id, COALESCE(question_media_url, ''), COALESCE(question_media_type, '') FROM questions WHERE quiz_id = ? ORDER BY id", quizID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	questions := make([]QuestionWithOptions, 0, 16)
	questionIndex := make(map[int]int)
	for rows.Next() {
		var qst question
		var questionMediaURL, questionMediaType string
		if err := rows.Scan(&qst.Id, &qst.Text, &qst.Type, &qst.Points, &qst.AuthorID, &qst.QuizID, &questionMediaURL, &questionMediaType); err != nil {
			return nil, err
		}

		qst.Type = strings.ToLower(strings.TrimSpace(qst.Type))
		qst.Text = strings.TrimSpace(qst.Text)
		qst.QuestionMediaURL, qst.QuestionMediaType = normalizeQuestionMedia(questionMediaURL, questionMediaType)

		questionIndex[qst.Id] = len(questions)
		questions = append(questions, QuestionWithOptions{Question: qst})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(questions) == 0 {
		return questions, nil
	}

	optionRows, err := db.Query(`SELECT qo.question_id, qo.id, qo.option_text, qo.option_image_url, qo.is_correct
		FROM question_options qo
		JOIN questions q ON q.id = qo.question_id
		WHERE q.quiz_id = ?
		ORDER BY qo.question_id, qo.id`, quizID)
	if err != nil {
		return nil, err
	}
	defer optionRows.Close()

	acceptedSeen := make(map[int]map[string]struct{})
	acceptedAnswers := make(map[int][]string)
	for optionRows.Next() {
		var questionID int
		var img sql.NullString
		opt := struct {
			Id         int
			OptionText string
			ImageURL   string
			IsCorrect  bool
		}{}
		if err := optionRows.Scan(&questionID, &opt.Id, &opt.OptionText, &img, &opt.IsCorrect); err != nil {
			return nil, err
		}

		idx, ok := questionIndex[questionID]
		if !ok {
			continue
		}
		opt.ImageURL = nullStringToString(img)
		opt.OptionText = strings.TrimSpace(opt.OptionText)

		qType := questions[idx].Question.Type
		if isChoiceQuestionType(qType) {
			questions[idx].Options = append(questions[idx].Options, opt)
			continue
		}
		if isOpenQuestionType(qType) && opt.IsCorrect && opt.OptionText != "" {
			norm := normalizeOpenAnswer(opt.OptionText)
			if norm == "" {
				continue
			}
			if acceptedSeen[questionID] == nil {
				acceptedSeen[questionID] = make(map[string]struct{}, 4)
			}
			if _, exists := acceptedSeen[questionID][norm]; exists {
				continue
			}
			acceptedSeen[questionID][norm] = struct{}{}
			acceptedAnswers[questionID] = append(acceptedAnswers[questionID], opt.OptionText)
		}
	}
	if err := optionRows.Err(); err != nil {
		return nil, err
	}

	for questionID, answers := range acceptedAnswers {
		if idx, ok := questionIndex[questionID]; ok && len(answers) > 0 {
			questions[idx].OpenAnswersText = strings.Join(answers, "\n")
		}
	}

	return questions, nil
}

func handleHomePage(w http.ResponseWriter, r *http.Request) {
	username, isLoggedIn := getCurrentUsername(r)
	searchBar := strings.TrimSpace(r.URL.Query().Get("q"))
	category := strings.TrimSpace(r.URL.Query().Get("category"))
	if category == "" {
		category = "all"
	}
	limit := 6
	where := "privacy = 'public' AND status = 'open'"
	if searchBar != "" || category != "all" {
		where = "status = 'open'"
	} else {
		limit = 24
	}
	args := make([]any, 0, 4)

	if searchBar != "" {
		like := "%" + searchBar + "%"
		if id, err := strconv.Atoi(searchBar); err == nil {
			where += " AND (id = ? OR title LIKE ? OR author LIKE ?)"
			args = append(args, id, like, like)
		} else {
			where += " AND (title LIKE ? OR author LIKE ?)"
			args = append(args, like, like)
		}
	}

	if category != "" && category != "all" {
		where += " AND category = ?"
		args = append(args, category)
	}

	query := fmt.Sprintf("SELECT "+quizSelectColumns+" FROM quizzes WHERE %s ORDER BY id DESC LIMIT %d;", where, limit)

	rows, err := db.Query(query, args...)
	if err != nil {
		panic(err.Error())
		return
	}
	defer rows.Close()

	var quizzes []quiz
	for rows.Next() {
		row := quiz{}
		err = rows.Scan(&row.Id, &row.Title, &row.Description, &row.Category, &row.Author, &row.PrivateKey, &row.Privacy, &row.Status, &row.TimeLimitSeconds, &row.PassScore, &row.ParticipationMode, &row.TeamCount)
		if err != nil {
			panic(err.Error())
			return
		}
		row.ParticipationMode = normalizeParticipationMode(row.ParticipationMode)
		row.TeamCount = normalizeTeamCount(row.ParticipationMode, row.TeamCount)
		quizzes = append(quizzes, row)
	}

	data := struct {
		Quizzes          []quiz
		Username         string
		IsLoggedIn       bool
		SearchQuery      string
		SelectedCategory string
	}{
		Quizzes:          quizzes,
		Username:         username,
		IsLoggedIn:       isLoggedIn,
		SearchQuery:      searchBar,
		SelectedCategory: category,
	}

	tmpl, _ := template.ParseFiles("frontend/homePage.html")
	err = tmpl.Execute(w, data)
	if err != nil {
		panic(err.Error())
	}
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		tmpl, _ := template.ParseFiles("frontend/loginForm.html")
		data := struct {
			Error   string
			Success string
		}{}
		if r.URL.Query().Get("success") == "1" {
			data.Success = "Регистрация успешна"
		}
		err := tmpl.Execute(w, data)
		if err != nil {
			panic(err.Error())
		}
	} else if r.Method == "POST" {
		username := r.FormValue("username")
		password := r.FormValue("password")
		var dbPassword string
		var userID int
		err := db.QueryRow("SELECT id, password FROM users WHERE username = ?", username).Scan(&userID, &dbPassword)
		data := struct {
			Error   string
			Success string
		}{}
		if err != nil {
			data.Error = "Пользователь не найден"
		} else if password == dbPassword {
			session, _ := store.Get(r, "session-name")
			session.Values["authenticated"] = true
			session.Values["userID"] = userID
			session.Values["username"] = username
			session.Save(r, w)
			http.Redirect(w, r, "/", http.StatusSeeOther)
		} else {
			data.Error = "Неверный пароль"
		}

		tmpl, _ := template.ParseFiles("frontend/loginForm.html")
		err = tmpl.Execute(w, data)
		if err != nil {
			panic(err.Error())
		}
	}
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	session, _ := store.Get(r, "session-name")
	session.Values["authenticated"] = false
	session.Values["userID"] = nil
	session.Values["username"] = nil
	session.Save(r, w)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		tmpl, _ := template.ParseFiles("frontend/registerForm.html")
		err := tmpl.Execute(w, nil)
		if err != nil {
			panic(err.Error())
		}
		return
	} else if r.Method == "POST" {
		username := r.FormValue("username")
		password := r.FormValue("password")
		var exists int
		err := db.QueryRow("SELECT COUNT(*) FROM users WHERE username = ?", username).Scan(&exists)
		if err != nil {
			http.Error(w, "Ошибка базы данных", http.StatusInternalServerError)
			return
		}
		if exists > 0 {
			data := struct {
				Error string
			}{
				Error: "Имя занято",
			}
			tmpl, _ := template.ParseFiles("frontend/registerForm.html")
			err = tmpl.Execute(w, data)
			if err != nil {
				panic(err.Error())
			}
			return
		}
		_, _ = db.Exec("INSERT INTO users (username, password) VALUES (?, ?)", username, password)
		http.Redirect(w, r, "/login?success=1", http.StatusSeeOther)
	}
}

func handleValidateQuizPrivateKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "valid": false, "error": "Метод не поддерживается"})
		return
	}

	if err := r.ParseForm(); err != nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "valid": false, "error": "Некорректные данные"})
		return
	}

	quizID, err := strconv.Atoi(strings.TrimSpace(r.FormValue("quiz_id")))
	if err != nil || quizID <= 0 {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "valid": false, "error": "Некорректный ID квиза"})
		return
	}

	key := strings.TrimSpace(r.FormValue("key"))
	if key == "" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "valid": false, "error": "Введите приватный ключ"})
		return
	}

	var privacy, privateKey, status string
	err = db.QueryRow("SELECT privacy, private_key, status FROM quizzes WHERE id = ?", quizID).Scan(&privacy, &privateKey, &status)
	if err != nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if errors.Is(err, sql.ErrNoRows) {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "valid": false, "error": "Квиз не найден"})
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "valid": false, "error": "Ошибка базы данных"})
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	if status == "closed" {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "valid": false, "error": "Квиз закрыт"})
		return
	}

	if privacy != "private" {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "valid": true})
		return
	}

	if key != privateKey {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "valid": false, "error": "Неверный ключ"})
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "valid": true})
}

func handleViewQuiz(w http.ResponseWriter, r *http.Request) {
	quizID, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/quiz/"))
	if err != nil {
		http.Error(w, "Некорректный ID квиза", http.StatusBadRequest)
		return
	}

	var q quiz
	err = db.QueryRow("SELECT "+quizSelectColumns+" FROM quizzes WHERE id = ?", quizID).Scan(
		&q.Id, &q.Title, &q.Description, &q.Category, &q.Author, &q.PrivateKey, &q.Privacy, &q.Status, &q.TimeLimitSeconds, &q.PassScore, &q.ParticipationMode, &q.TeamCount,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
		} else {
			log.Printf("Ошибка чтения квиза %d: %v", quizID, err)
			http.Error(w, "Ошибка базы данных", http.StatusInternalServerError)
		}
		return
	}
	q.ParticipationMode = normalizeParticipationMode(q.ParticipationMode)
	q.TeamCount = normalizeTeamCount(q.ParticipationMode, q.TeamCount)

	if q.Status == "closed" {
		http.Error(w, "Квиз закрыт", http.StatusForbidden)
		return
	}

	type optionReviewView struct {
		ID         int
		Text       string
		ImageURL   string
		IsCorrect  bool
		IsSelected bool
	}

	type questionReviewView struct {
		Index             int
		QuestionID        int
		Text              string
		Type              string
		TypeLabel         string
		Points            int
		Earned            int
		Status            string
		StatusLabel       string
		UserAnswerText    string
		AcceptedAnswers   []string
		QuestionMediaURL  string
		QuestionMediaType string
		Options           []optionReviewView
	}

	type pageData struct {
		Quiz            quiz
		Questions       []QuestionWithOptions
		StartedAt       string
		Result          *QuizResult
		Review          any
		RequiresKey     bool
		KeyError        string
		ProvidedKey     string
		ParticipantName string
		TeamName        string
		TeamNumber      int
		EntryError      string
		TeamLeaderboard []TeamStanding
	}

	renderPage := func(statusCode int, data pageData) {
		tmpl, err := template.ParseFiles("frontend/quizPage.html")
		if err != nil {
			log.Printf("Ошибка загрузки шаблона quizPage.html: %v", err)
			http.Error(w, "Ошибка шаблона", http.StatusInternalServerError)
			return
		}
		if statusCode > 0 {
			w.WriteHeader(statusCode)
		}
		if err := tmpl.Execute(w, data); err != nil {
			log.Printf("Ошибка отображения страницы квиза %d: %v", quizID, err)
		}
	}

	parsePositiveInt := func(raw string) int {
		v, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || v <= 0 {
			return 0
		}
		return v
	}

	participantFromRequest := func() string {
		name := strings.TrimSpace(r.URL.Query().Get("participant_name"))
		if name == "" {
			name = strings.TrimSpace(r.URL.Query().Get("player_name"))
		}
		if r.Method == http.MethodPost {
			if posted := strings.TrimSpace(r.FormValue("participant_name")); posted != "" {
				name = posted
			}
		}
		if len([]rune(name)) > 64 {
			r := []rune(name)
			name = string(r[:64])
		}
		return name
	}

	teamNameFromRequest := func() string {
		teamName := strings.TrimSpace(r.URL.Query().Get("team_name"))
		if r.Method == http.MethodPost {
			if posted := strings.TrimSpace(r.FormValue("team_name")); posted != "" {
				teamName = posted
			}
		}
		return sanitizeTeamName(teamName)
	}

	teamNumberFromRequest := func() int {
		n := parsePositiveInt(r.URL.Query().Get("team_number"))
		if r.Method == http.MethodPost {
			if posted := parsePositiveInt(r.FormValue("team_number")); posted > 0 {
				n = posted
			}
		}
		return n
	}

	providedKey := r.URL.Query().Get("key")
	if providedKey == "" {
		_ = r.ParseForm()
		providedKey = r.FormValue("key")
	}
	isUnlocked := (q.Privacy != "private") || (providedKey != "" && providedKey == q.PrivateKey)

	participantName := participantFromRequest()
	teamName := teamNameFromRequest()
	teamNumber := teamNumberFromRequest()

	session, _ := store.Get(r, "session-name")
	if session != nil && session.Values != nil {
		if participantName != "" {
			session.Values[fmt.Sprintf("quiz_participant_name_%d", quizID)] = participantName
		}
		if teamName != "" {
			session.Values[fmt.Sprintf("quiz_team_name_%d", quizID)] = teamName
		}
		if teamNumber > 0 {
			session.Values[fmt.Sprintf("quiz_team_number_%d", quizID)] = teamNumber
		}
		if participantName != "" || teamName != "" || teamNumber > 0 {
			_ = session.Save(r, w)
		}
	}

	if q.Privacy == "private" && !isUnlocked && r.Method == http.MethodGet {
		renderPage(0, pageData{
			Quiz:        q,
			Questions:   nil,
			StartedAt:   time.Now().Format("2006-01-02 15:04:05"),
			Result:      nil,
			Review:      nil,
			RequiresKey: true,
			KeyError: func() string {
				if r.URL.Query().Get("key") != "" {
					return "Неверный приватный ключ"
				}
				return ""
			}(),
			ProvidedKey:     providedKey,
			ParticipantName: participantName,
			TeamName:        teamName,
			TeamNumber:      teamNumber,
		})
		return
	}

	if q.Privacy == "private" && !isUnlocked && r.Method == http.MethodPost {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("Неверный приватный ключ"))
		return
	}

	questions, err := loadQuizQuestionsWithOptions(quizID)
	if err != nil {
		log.Printf("Ошибка загрузки вопросов квиза %d: %v", quizID, err)
		http.Error(w, "Ошибка базы данных", http.StatusInternalServerError)
		return
	}

	if r.Method == http.MethodGet {
		userID, _ := getUserID(r)
		if isTeamRouletteMode(q.ParticipationMode) && participantName != "" && teamName == "" {
			teamNumber, teamName = latestRouletteTeamForParticipant(quizID, participantName, userID, q.TeamCount)
			if session != nil && session.Values != nil {
				session.Values[fmt.Sprintf("quiz_team_name_%d", quizID)] = teamName
				session.Values[fmt.Sprintf("quiz_team_number_%d", quizID)] = teamNumber
				_ = session.Save(r, w)
			}
		}
		renderPage(0, pageData{
			Quiz:            q,
			Questions:       questions,
			StartedAt:       time.Now().Format("2006-01-02 15:04:05"),
			Result:          nil,
			Review:          nil,
			RequiresKey:     false,
			KeyError:        "",
			ProvidedKey:     providedKey,
			ParticipantName: participantName,
			TeamName:        teamName,
			TeamNumber:      teamNumber,
		})
		return
	}

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	userID, _ := getUserID(r)

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Некорректные данные формы", http.StatusBadRequest)
		return
	}

	participantName = strings.TrimSpace(r.FormValue("participant_name"))
	if participantName == "" {
		participantName = strings.TrimSpace(r.URL.Query().Get("participant_name"))
		if participantName == "" {
			participantName = strings.TrimSpace(r.URL.Query().Get("player_name"))
		}
	}
	if participantName == "" && session != nil && session.Values != nil {
		if v, ok := session.Values[fmt.Sprintf("quiz_participant_name_%d", quizID)].(string); ok {
			participantName = strings.TrimSpace(v)
		}
	}
	participantName = sanitizeParticipantName(participantName)

	mode := normalizeParticipationMode(q.ParticipationMode)
	teamName = sanitizeTeamName(strings.TrimSpace(r.FormValue("team_name")))
	teamNumber = parsePositiveInt(r.FormValue("team_number"))
	if teamName == "" {
		teamName = sanitizeTeamName(strings.TrimSpace(r.URL.Query().Get("team_name")))
		if teamName == "" && mode != "team_roulette" && session != nil && session.Values != nil {
			if v, ok := session.Values[fmt.Sprintf("quiz_team_name_%d", quizID)].(string); ok {
				teamName = sanitizeTeamName(v)
			}
		}
	}
	if teamNumber <= 0 {
		teamNumber = parsePositiveInt(r.URL.Query().Get("team_number"))
		if teamNumber <= 0 && mode != "team_roulette" && session != nil && session.Values != nil {
			if v, ok := session.Values[fmt.Sprintf("quiz_team_number_%d", quizID)].(int); ok && v > 0 {
				teamNumber = v
			}
		}
	}

	switch mode {
	case "team_manual":
		if teamName == "" {
			renderPage(http.StatusBadRequest, pageData{
				Quiz:            q,
				Questions:       questions,
				StartedAt:       time.Now().Format("2006-01-02 15:04:05"),
				Result:          nil,
				Review:          nil,
				RequiresKey:     false,
				ProvidedKey:     providedKey,
				ParticipantName: participantName,
				TeamName:        "",
				EntryError:      "Укажите название команды.",
			})
			return
		}
		teamNumber = 0
	case "team_roulette":
		if teamNumber <= 0 || teamNumber > q.TeamCount {
			teamNumber, teamName = latestRouletteTeamForParticipant(quizID, participantName, userID, q.TeamCount)
		}
		if teamName == "" {
			teamName = rouletteTeamName(teamNumber)
		}
	default:
		teamName = ""
		teamNumber = 0
	}

	if session != nil && session.Values != nil {
		session.Values[fmt.Sprintf("quiz_participant_name_%d", quizID)] = participantName
		if teamName != "" {
			session.Values[fmt.Sprintf("quiz_team_name_%d", quizID)] = teamName
		}
		if teamNumber > 0 {
			session.Values[fmt.Sprintf("quiz_team_number_%d", quizID)] = teamNumber
		}
		_ = session.Save(r, w)
	}

	if q.Privacy == "private" {
		k := strings.TrimSpace(r.URL.Query().Get("key"))
		if k == "" {
			k = strings.TrimSpace(r.FormValue("key"))
		}
		if k == "" || k != q.PrivateKey {
			renderPage(http.StatusForbidden, pageData{
				Quiz:            q,
				Questions:       nil,
				StartedAt:       time.Now().Format("2006-01-02 15:04:05"),
				Result:          nil,
				Review:          nil,
				RequiresKey:     true,
				KeyError:        "Неверный приватный ключ",
				ProvidedKey:     k,
				ParticipantName: participantName,
				TeamName:        teamName,
				TeamNumber:      teamNumber,
			})
			return
		}
		providedKey = k
	}

	startedAt := time.Now()
	if s := strings.TrimSpace(r.FormValue("started_at")); s != "" {
		if t, perr := time.ParseInLocation("2006-01-02 15:04:05", s, time.Local); perr == nil {
			startedAt = t
		}
	}
	finishedAt := time.Now()
	if q.TimeLimitSeconds > 0 {
		deadline := startedAt.Add(time.Duration(q.TimeLimitSeconds) * time.Second)
		if finishedAt.After(deadline) {
			finishedAt = deadline
		}
	}

	attempt := 1
	if name := normalizeParticipantIdentityName(participantName); name != "" {
		_ = db.QueryRow(`SELECT COALESCE(MAX(attempt_number), 0) + 1
			FROM quiz_sessions
			WHERE quiz_id = ? AND LOWER(TRIM(participant_name)) = ?`, quizID, name).Scan(&attempt)
	} else if userID > 0 {
		_ = db.QueryRow("SELECT COALESCE(MAX(attempt_number), 0) + 1 FROM quiz_sessions WHERE quiz_id = ? AND user_id = ?", quizID, userID).Scan(&attempt)
	}

	score := 0
	maxScore := 0
	answerRows := make([]struct {
		QuestionID        int
		SelectedOptionIDs []int
		AnswerText        string
		IsCorrect         *bool
	}, 0, len(questions))
	review := make([]questionReviewView, 0, len(questions))

	typeLabel := func(t string) string {
		switch t {
		case "single":
			return "Одиночный выбор"
		case "multiple":
			return "Множественный выбор"
		case "photo_single":
			return "Фото-одиночный выбор"
		case "photo_multiple":
			return "Фото-множественный выбор"
		case "open":
			return "Открытый вопрос"
		case "photo":
			return "Фото"
		case "video":
			return "Видео"
		default:
			return t
		}
	}

	for i, qw := range questions {
		qid := qw.Question.Id
		qType := qw.Question.Type
		points := qw.Question.Points

		if isOpenQuestionType(qType) {
			userText := strings.TrimSpace(r.FormValue(fmt.Sprintf("q_%d_text", qid)))
			acceptedAnswers := make([]string, 0, 4)
			acceptedSeen := make(map[string]struct{}, 4)
			for _, line := range strings.Split(qw.OpenAnswersText, "\n") {
				clean := strings.TrimSpace(line)
				if clean == "" {
					continue
				}
				norm := normalizeOpenAnswer(clean)
				if norm == "" {
					continue
				}
				if _, ok := acceptedSeen[norm]; ok {
					continue
				}
				acceptedSeen[norm] = struct{}{}
				acceptedAnswers = append(acceptedAnswers, clean)
			}
			if len(acceptedAnswers) == 0 {
				answerRows = append(answerRows, struct {
					QuestionID        int
					SelectedOptionIDs []int
					AnswerText        string
					IsCorrect         *bool
				}{QuestionID: qid, AnswerText: userText, IsCorrect: nil})
				review = append(review, questionReviewView{
					Index:             i + 1,
					QuestionID:        qid,
					Text:              qw.Question.Text,
					Type:              qType,
					TypeLabel:         typeLabel(qType),
					Points:            points,
					Earned:            0,
					Status:            "unchecked",
					StatusLabel:       "Нет заданного правильного ответа",
					UserAnswerText:    userText,
					AcceptedAnswers:   acceptedAnswers,
					QuestionMediaURL:  qw.Question.QuestionMediaURL,
					QuestionMediaType: qw.Question.QuestionMediaType,
				})
				continue
			}
			maxScore += points
			isCorrect := false
			userNorm := normalizeOpenAnswer(userText)
			if userNorm != "" {
				for _, ans := range acceptedAnswers {
					if userNorm == normalizeOpenAnswer(ans) {
						isCorrect = true
						break
					}
				}
			}
			earned := 0
			status := "wrong"
			statusLabel := "Неверно"
			if isCorrect {
				score += points
				earned = points
				status = "correct"
				statusLabel = "Верно"
			}
			ic := isCorrect
			answerRows = append(answerRows, struct {
				QuestionID        int
				SelectedOptionIDs []int
				AnswerText        string
				IsCorrect         *bool
			}{QuestionID: qid, AnswerText: userText, IsCorrect: &ic})
			review = append(review, questionReviewView{
				Index:             i + 1,
				QuestionID:        qid,
				Text:              qw.Question.Text,
				Type:              qType,
				TypeLabel:         typeLabel(qType),
				Points:            points,
				Earned:            earned,
				Status:            status,
				StatusLabel:       statusLabel,
				UserAnswerText:    userText,
				AcceptedAnswers:   acceptedAnswers,
				QuestionMediaURL:  qw.Question.QuestionMediaURL,
				QuestionMediaType: qw.Question.QuestionMediaType,
			})
			continue
		}

		if !isChoiceQuestionType(qType) {
			answerRows = append(answerRows, struct {
				QuestionID        int
				SelectedOptionIDs []int
				AnswerText        string
				IsCorrect         *bool
			}{QuestionID: qid, IsCorrect: nil})
			review = append(review, questionReviewView{
				Index:             i + 1,
				QuestionID:        qid,
				Text:              qw.Question.Text,
				Type:              qType,
				TypeLabel:         typeLabel(qType),
				Points:            points,
				Earned:            0,
				Status:            "unchecked",
				StatusLabel:       "Не проверяется автоматически",
				QuestionMediaURL:  qw.Question.QuestionMediaURL,
				QuestionMediaType: qw.Question.QuestionMediaType,
			})
			continue
		}

		correctIDs := make([]int, 0, 4)
		for _, opt := range qw.Options {
			if opt.IsCorrect {
				correctIDs = append(correctIDs, opt.Id)
			}
		}

		field := fmt.Sprintf("q_%d", qid)
		vals := r.Form[field]
		selected := make([]int, 0, len(vals))
		for _, v := range vals {
			id, err := strconv.Atoi(strings.TrimSpace(v))
			if err == nil && id > 0 {
				selected = append(selected, id)
			}
		}
		selected = uniqueSortedInts(selected)

		if len(correctIDs) == 0 {
			answerRows = append(answerRows, struct {
				QuestionID        int
				SelectedOptionIDs []int
				AnswerText        string
				IsCorrect         *bool
			}{QuestionID: qid, SelectedOptionIDs: selected, IsCorrect: nil})
			optSel := makeIntSet(selected)
			opts := make([]optionReviewView, 0, len(qw.Options))
			for _, opt := range qw.Options {
				_, isSel := optSel[opt.Id]
				opts = append(opts, optionReviewView{ID: opt.Id, Text: opt.OptionText, ImageURL: opt.ImageURL, IsCorrect: false, IsSelected: isSel})
			}
			review = append(review, questionReviewView{
				Index:             i + 1,
				QuestionID:        qid,
				Text:              qw.Question.Text,
				Type:              qType,
				TypeLabel:         typeLabel(qType),
				Points:            points,
				Earned:            0,
				Status:            "unchecked",
				StatusLabel:       "Нет заданного правильного ответа",
				QuestionMediaURL:  qw.Question.QuestionMediaURL,
				QuestionMediaType: qw.Question.QuestionMediaType,
				Options:           opts,
			})
			continue
		}

		maxScore += points
		isCorrect := false
		if isSingleChoiceQuestionType(qType) {
			if len(selected) == 1 {
				sel := selected[0]
				for _, cid := range correctIDs {
					if sel == cid {
						isCorrect = true
						break
					}
				}
			}
		} else if len(selected) > 0 {
			isCorrect = sameIntSet(selected, correctIDs)
		}

		earned := 0
		status := "wrong"
		statusLabel := "Неверно"
		if isCorrect {
			score += points
			earned = points
			status = "correct"
			statusLabel = "Верно"
		}
		ic := isCorrect
		answerRows = append(answerRows, struct {
			QuestionID        int
			SelectedOptionIDs []int
			AnswerText        string
			IsCorrect         *bool
		}{QuestionID: qid, SelectedOptionIDs: selected, IsCorrect: &ic})

		optSel := makeIntSet(selected)
		opts := make([]optionReviewView, 0, len(qw.Options))
		for _, opt := range qw.Options {
			_, sel := optSel[opt.Id]
			opts = append(opts, optionReviewView{ID: opt.Id, Text: opt.OptionText, ImageURL: opt.ImageURL, IsCorrect: opt.IsCorrect, IsSelected: sel})
		}
		review = append(review, questionReviewView{
			Index:             i + 1,
			QuestionID:        qid,
			Text:              qw.Question.Text,
			Type:              qType,
			TypeLabel:         typeLabel(qType),
			Points:            points,
			Earned:            earned,
			Status:            status,
			StatusLabel:       statusLabel,
			QuestionMediaURL:  qw.Question.QuestionMediaURL,
			QuestionMediaType: qw.Question.QuestionMediaType,
			Options:           opts,
		})
	}

	tx, err := db.Begin()
	if err != nil {
		log.Printf("Не удалось начать транзакцию прохождения квиза %d: %v", quizID, err)
		http.Error(w, "Ошибка базы данных", http.StatusInternalServerError)
		return
	}
	defer func() { _ = tx.Rollback() }()

	failSubmitDB := func(message string, err error) {
		log.Printf("%s для квиза %d: %v", message, quizID, err)
		http.Error(w, "Ошибка базы данных", http.StatusInternalServerError)
	}

	sessionUserID := sql.NullInt64{Int64: int64(userID), Valid: userID > 0}
	res, err := tx.Exec(
		"INSERT INTO quiz_sessions (quiz_id, user_id, participant_name, team_name, team_number, attempt_number, started_at, finished_at, score, max_score) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		quizID, sessionUserID, participantName, sql.NullString{String: teamName, Valid: teamName != ""}, sql.NullInt64{Int64: int64(teamNumber), Valid: teamNumber > 0}, attempt, startedAt, finishedAt, score, maxScore,
	)
	if err != nil {
		failSubmitDB("Не удалось создать quiz_session", err)
		return
	}
	sessionID, _ := res.LastInsertId()

	for _, ar := range answerRows {
		if ar.IsCorrect == nil {
			if len(ar.SelectedOptionIDs) == 0 {
				_, err = tx.Exec(
					"INSERT INTO user_answers (session_id, question_id, selected_option_id, answer_text, is_correct) VALUES (?, ?, NULL, ?, NULL)",
					sessionID, ar.QuestionID, ar.AnswerText,
				)
				if err != nil {
					failSubmitDB("Не удалось вставить ответ", err)
					return
				}
				continue
			}
			for _, oid := range ar.SelectedOptionIDs {
				_, err = tx.Exec(
					"INSERT INTO user_answers (session_id, question_id, selected_option_id, answer_text, is_correct) VALUES (?, ?, ?, NULL, NULL)",
					sessionID, ar.QuestionID, oid,
				)
				if err != nil {
					failSubmitDB("Не удалось вставить ответ", err)
					return
				}
			}
			continue
		}
		if len(ar.SelectedOptionIDs) == 0 {
			_, err = tx.Exec(
				"INSERT INTO user_answers (session_id, question_id, selected_option_id, answer_text, is_correct) VALUES (?, ?, NULL, ?, ?)",
				sessionID, ar.QuestionID, ar.AnswerText, *ar.IsCorrect,
			)
			if err != nil {
				failSubmitDB("Не удалось вставить пустой ответ", err)
				return
			}
			continue
		}
		for _, oid := range ar.SelectedOptionIDs {
			_, err = tx.Exec(
				"INSERT INTO user_answers (session_id, question_id, selected_option_id, answer_text, is_correct) VALUES (?, ?, ?, NULL, ?)",
				sessionID, ar.QuestionID, oid, *ar.IsCorrect,
			)
			if err != nil {
				failSubmitDB("Не удалось вставить ответ", err)
				return
			}
		}
	}

	if err := tx.Commit(); err != nil {
		log.Printf("Не удалось завершить транзакцию прохождения квиза %d: %v", quizID, err)
		http.Error(w, "Ошибка базы данных", http.StatusInternalServerError)
		return
	}

	passed := true
	if q.PassScore > 0 {
		passed = score >= q.PassScore
	}
	durationSeconds := int(finishedAt.Sub(startedAt).Seconds())
	if durationSeconds < 0 {
		durationSeconds = 0
	}

	teamLeaderboard := make([]TeamStanding, 0)
	teamRank := 0
	teamsTotal := 0
	if isTeamMode(q.ParticipationMode) && teamName != "" {
		if standings, rank, err := buildQuizTeamLeaderboard(quizID, teamName, isTeamRouletteMode(q.ParticipationMode)); err == nil {
			teamsTotal = len(standings)
			if len(standings) > 10 {
				teamLeaderboard = standings[:10]
			} else {
				teamLeaderboard = standings
			}
			teamRank = rank
		}
	}

	renderPage(0, pageData{
		Quiz:      q,
		Questions: questions,
		StartedAt: startedAt.Format("2006-01-02 15:04:05"),
		Result: &QuizResult{
			SessionID:       sessionID,
			AttemptNumber:   attempt,
			Score:           score,
			MaxScore:        maxScore,
			DurationSeconds: durationSeconds,
			DurationHuman:   formatDurationHuman(time.Duration(durationSeconds) * time.Second),
			Passed:          passed,
			HasPassScore:    q.PassScore > 0,
			PassScore:       q.PassScore,
			TeamName:        teamName,
			TeamRank:        teamRank,
			TeamsTotal:      teamsTotal,
		},
		Review:          review,
		RequiresKey:     false,
		KeyError:        "",
		ProvidedKey:     providedKey,
		ParticipantName: participantName,
		TeamName:        teamName,
		TeamNumber:      teamNumber,
		TeamLeaderboard: teamLeaderboard,
	})
}

func handleQuizStatusToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		if wantsJSON(r) {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
			return
		}
		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		return
	}
	if !isAuthenticated(r) {
		if wantsJSON(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "unauthorized"})
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	username, _ := getCurrentUsername(r)
	quizID, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/api/quiz-status/"))
	if err != nil || quizID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid quiz id"})
		return
	}
	var author, currentStatus string
	err = db.QueryRow("SELECT author, status FROM quizzes WHERE id = ?", quizID).Scan(&author, &currentStatus)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "quiz not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "db error"})
		return
	}
	if author != username {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "forbidden"})
		return
	}
	newStatus := "closed"
	if currentStatus == "closed" {
		newStatus = "open"
	}
	if _, err := db.Exec("UPDATE quizzes SET status = ? WHERE id = ?", newStatus, quizID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "update failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": newStatus})
}

func handleMyQuizzes(w http.ResponseWriter, r *http.Request) {
	username, _ := getCurrentUsername(r)
	if username == strings.TrimPrefix(r.URL.Path, "/my-quizzes/") {
		rows, err := db.Query("SELECT "+quizSelectColumns+" FROM quizzes WHERE author = ? ORDER BY id DESC", username)
		if err != nil {
			panic(err.Error())
			return
		}
		defer rows.Close()
		var quizzes []quiz
		for rows.Next() {
			row := quiz{}
			err = rows.Scan(&row.Id, &row.Title, &row.Description, &row.Category, &row.Author, &row.PrivateKey, &row.Privacy, &row.Status, &row.TimeLimitSeconds, &row.PassScore, &row.ParticipationMode, &row.TeamCount)
			if err != nil {
				panic(err.Error())
				return
			}
			row.ParticipationMode = normalizeParticipationMode(row.ParticipationMode)
			row.TeamCount = normalizeTeamCount(row.ParticipationMode, row.TeamCount)
			quizzes = append(quizzes, row)
		}

		data := struct {
			Quizzes  []quiz
			Username string
		}{
			Quizzes:  quizzes,
			Username: username,
		}

		tmpl, _ := template.ParseFiles("frontend/myQuizzes.html")
		err = tmpl.Execute(w, data)
		if err != nil {
			panic(err.Error())
		}

	} else {
		fmt.Fprintf(w, "Страница не найдена")
	}
}

func defaultNewQuiz() quiz {
	return quiz{
		Category:          "language_quiz",
		Privacy:           "public",
		Status:            "open",
		ParticipationMode: "individual",
		TeamCount:         0,
		TimeLimitSeconds:  0,
		PassScore:         0,
	}
}

func renderEditQuizTemplate(w http.ResponseWriter, data editQuizPageData) {
	tmpl, err := template.New("editQuiz.html").Funcs(template.FuncMap{
		"inc": func(i int) int { return i + 1 },
	}).ParseFiles("frontend/editQuiz.html")
	if err != nil {
		log.Printf("Ошибка загрузки шаблона editQuiz.html: %v", err)
		http.Error(w, "Ошибка сервера", http.StatusInternalServerError)
		return
	}
	if err := tmpl.Execute(w, data); err != nil {
		log.Printf("Ошибка отображения editQuiz.html: %v", err)
		http.Error(w, "Ошибка сервера", http.StatusInternalServerError)
		return
	}
}

func insertQuizQuestionFromForm(tx *sql.Tx, userID int, quizID int64, questionText, questionType string, points int, mediaURL, mediaType, openAnswers string, options []quizFormOption) error {
	questionText = strings.TrimSpace(questionText)
	questionType = strings.ToLower(strings.TrimSpace(questionType))
	if questionType == "" {
		questionType = "single"
	}
	if questionText == "" && questionType != "video" {
		return nil
	}
	if points < 0 {
		points = 0
	}
	mediaURL, mediaType = normalizeQuestionMedia(mediaURL, mediaType)

	res, err := tx.Exec(
		"INSERT INTO questions (text, type, points, author_id, quiz_id, question_media_url, question_media_type) VALUES (?, ?, ?, ?, ?, ?, ?)",
		questionText, questionType, points, userID, quizID, mediaURL, mediaType,
	)
	if err != nil {
		return err
	}

	questionID, err := res.LastInsertId()
	if err != nil {
		return err
	}

	if isOpenQuestionType(questionType) {
		openAnswersSeen := make(map[string]struct{})
		for _, line := range strings.Split(openAnswers, "\n") {
			answerText := strings.TrimSpace(line)
			if answerText == "" {
				continue
			}
			normalized := normalizeOpenAnswer(answerText)
			if normalized == "" {
				continue
			}
			if _, exists := openAnswersSeen[normalized]; exists {
				continue
			}
			openAnswersSeen[normalized] = struct{}{}

			_, err = tx.Exec(
				"INSERT INTO question_options (question_id, option_text, option_image_url, is_correct) VALUES (?, ?, ?, ?)",
				questionID, answerText, "", true,
			)
			if err != nil {
				return err
			}
		}
		return nil
	}

	if !isChoiceQuestionType(questionType) {
		return nil
	}

	cleanOptions := make([]quizFormOption, 0, len(options))
	for _, opt := range options {
		opt.text = strings.TrimSpace(opt.text)
		opt.imageURL = strings.TrimSpace(opt.imageURL)
		if opt.text == "" && opt.imageURL == "" {
			continue
		}
		cleanOptions = append(cleanOptions, opt)
	}

	if isSingleChoiceQuestionType(questionType) && len(cleanOptions) > 0 {
		hasCorrect := false
		for idx := range cleanOptions {
			if !cleanOptions[idx].isCorrect {
				continue
			}
			if hasCorrect {
				cleanOptions[idx].isCorrect = false
				continue
			}
			hasCorrect = true
		}
		if !hasCorrect {
			cleanOptions[0].isCorrect = true
		}
	}

	for _, opt := range cleanOptions {
		_, err = tx.Exec(
			"INSERT INTO question_options (question_id, option_text, option_image_url, is_correct) VALUES (?, ?, ?, ?)",
			questionID, opt.text, opt.imageURL, opt.isCorrect,
		)
		if err != nil {
			return err
		}
	}

	return nil
}

func handleCreateQuizFromEdit(w http.ResponseWriter, r *http.Request, username string) {
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		if !errors.Is(err, http.ErrNotMultipart) {
			log.Printf("Некорректные данные формы создания квиза: %v", err)
			http.Error(w, "Некорректные данные формы", http.StatusBadRequest)
			return
		}
		if err := r.ParseForm(); err != nil {
			log.Printf("Некорректные данные формы создания квиза: %v", err)
			http.Error(w, "Некорректные данные формы", http.StatusBadRequest)
			return
		}
	}

	userID, ok := getUserID(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	title := strings.TrimSpace(r.FormValue("title"))
	description := strings.TrimSpace(r.FormValue("description"))
	if description == "" {
		description = strings.TrimSpace(r.FormValue("quiz_description"))
	}
	category := strings.TrimSpace(r.FormValue("category"))
	if title == "" {
		http.Error(w, "Название квиза не может быть пустым", http.StatusBadRequest)
		return
	}
	if category == "" {
		category = "other"
	}

	privacy := strings.TrimSpace(r.FormValue("privacy"))
	if privacy != "private" {
		privacy = "public"
	}
	privateKey := ""
	if privacy == "private" {
		privateKey = strings.TrimSpace(r.FormValue("private_key"))
	}

	status := strings.TrimSpace(r.FormValue("status"))
	if status != "closed" {
		status = "open"
	}

	timeLimitSeconds, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("time_limit_seconds")))
	if timeLimitSeconds < 0 {
		timeLimitSeconds = 0
	}
	passScore, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("pass_score")))
	if passScore < 0 {
		passScore = 0
	}

	participationMode := normalizeParticipationMode(r.FormValue("participation_mode"))
	if participationMode == "team_manual" && r.FormValue("random_team_assignment") != "" {
		participationMode = "team_roulette"
	}
	teamCount, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("team_count")))
	teamCount = normalizeTeamCount(participationMode, teamCount)

	tx, err := db.Begin()
	if err != nil {
		log.Printf("Не удалось начать транзакцию создания квиза: %v", err)
		http.Error(w, "Ошибка базы данных", http.StatusInternalServerError)
		return
	}
	defer func() { _ = tx.Rollback() }()

	fail := func(message string, err error) {
		if err != nil {
			log.Printf("%s: %v", message, err)
		} else {
			log.Print(message)
		}
		http.Error(w, "Ошибка базы данных при сохранении квиза", http.StatusInternalServerError)
	}

	res, err := tx.Exec(
		"INSERT INTO quizzes (title, description, category, author, privacy, private_key, status, time_limit_seconds, pass_score, participation_mode, team_count) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		title, description, category, username, privacy, privateKey, status, timeLimitSeconds, passScore, participationMode, teamCount,
	)
	if err != nil {
		fail("Не удалось создать квиз", err)
		return
	}

	quizID, err := res.LastInsertId()
	if err != nil {
		fail("Не удалось получить ID созданного квиза", err)
		return
	}

	newTotal, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("new_total_questions")))
	if newTotal < 0 {
		newTotal = 0
	}
	for i := 1; i <= newTotal; i++ {
		qPoints, _ := strconv.Atoi(strings.TrimSpace(r.FormValue(fmt.Sprintf("new_question_points_%d", i))))
		optCount, _ := strconv.Atoi(strings.TrimSpace(r.FormValue(fmt.Sprintf("new_option_count_%d", i))))
		if optCount < 0 {
			optCount = 0
		}
		options := make([]quizFormOption, 0, optCount)
		for j := 1; j <= optCount; j++ {
			options = append(options, quizFormOption{
				text:      r.FormValue(fmt.Sprintf("new_option_%d_%d", i, j)),
				imageURL:  r.FormValue(fmt.Sprintf("new_option_img_%d_%d", i, j)),
				isCorrect: r.FormValue(fmt.Sprintf("new_is_correct_%d_%d", i, j)) != "",
			})
		}

		if err := insertQuizQuestionFromForm(
			tx,
			userID,
			quizID,
			r.FormValue(fmt.Sprintf("new_question_text_%d", i)),
			r.FormValue(fmt.Sprintf("new_question_type_%d", i)),
			qPoints,
			r.FormValue(fmt.Sprintf("new_question_media_url_%d", i)),
			r.FormValue(fmt.Sprintf("new_question_media_type_%d", i)),
			r.FormValue(fmt.Sprintf("new_open_answer_%d", i)),
			options,
		); err != nil {
			fail("Не удалось сохранить новый вопрос", err)
			return
		}
	}

	legacyQuestionIndexes := make([]int, 0)
	seenLegacyQuestionIndexes := make(map[int]struct{})
	for key := range r.PostForm {
		if !strings.HasPrefix(key, "question_text_") {
			continue
		}
		idx, err := strconv.Atoi(strings.TrimPrefix(key, "question_text_"))
		if err != nil || idx <= 0 {
			continue
		}
		if _, exists := seenLegacyQuestionIndexes[idx]; exists {
			continue
		}
		seenLegacyQuestionIndexes[idx] = struct{}{}
		legacyQuestionIndexes = append(legacyQuestionIndexes, idx)
	}
	sort.Ints(legacyQuestionIndexes)

	if len(legacyQuestionIndexes) == 0 {
		questionsAmount, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("total_questions")))
		for i := 1; i <= questionsAmount; i++ {
			legacyQuestionIndexes = append(legacyQuestionIndexes, i)
		}
	}

	for _, i := range legacyQuestionIndexes {
		qPoints, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("question_points_" + strconv.Itoa(i))))
		optCount, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("option_count_" + strconv.Itoa(i))))
		if optCount < 0 {
			optCount = 0
		}
		options := make([]quizFormOption, 0, optCount)
		for j := 1; j <= optCount; j++ {
			options = append(options, quizFormOption{
				text:      r.FormValue("option_" + strconv.Itoa(i) + "_" + strconv.Itoa(j)),
				imageURL:  r.FormValue("option_img_" + strconv.Itoa(i) + "_" + strconv.Itoa(j)),
				isCorrect: r.FormValue("is_correct_"+strconv.Itoa(i)+"_"+strconv.Itoa(j)) != "",
			})
		}

		if err := insertQuizQuestionFromForm(
			tx,
			userID,
			quizID,
			r.FormValue("question_text_"+strconv.Itoa(i)),
			r.FormValue("question_type_"+strconv.Itoa(i)),
			qPoints,
			r.FormValue("question_media_url_"+strconv.Itoa(i)),
			r.FormValue("question_media_type_"+strconv.Itoa(i)),
			r.FormValue("open_answer_"+strconv.Itoa(i)),
			options,
		); err != nil {
			fail("Не удалось сохранить вопрос", err)
			return
		}
	}

	if err := tx.Commit(); err != nil {
		fail("Не удалось завершить транзакцию создания квиза", err)
		return
	}

	http.Redirect(w, r, "/edit-quiz/"+strconv.FormatInt(quizID, 10), http.StatusSeeOther)
}

func handleEditQuiz(w http.ResponseWriter, r *http.Request) {
	if !isAuthenticated(r) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	username, ok := getCurrentUsername(r)
	if !ok || username == "" {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	rawQuizID := strings.Trim(strings.TrimPrefix(r.URL.Path, "/edit-quiz"), "/")
	if rawQuizID == "" {
		if r.Method == http.MethodPost {
			handleCreateQuizFromEdit(w, r, username)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
			return
		}
		renderEditQuizTemplate(w, editQuizPageData{
			Username:      username,
			Quiz:          defaultNewQuiz(),
			Questions:     nil,
			ExistingCount: 0,
			IsNew:         true,
		})
		return
	}

	quizID, err := strconv.Atoi(rawQuizID)
	if err != nil || quizID <= 0 {
		http.Error(w, "Некорректный ID квиза", http.StatusBadRequest)
		return
	}

	var author string
	err = db.QueryRow("SELECT author FROM quizzes WHERE id = ?", quizID).Scan(&author)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		log.Printf("Ошибка чтения автора квиза %d: %v", quizID, err)
		http.Error(w, "Ошибка базы данных", http.StatusInternalServerError)
		return
	}

	if username != author {
		http.NotFound(w, r)
		return
	}

	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			log.Printf("Некорректные данные формы редактирования квиза %d: %v", quizID, err)
			http.Error(w, "Некорректные данные формы", http.StatusBadRequest)
			return
		}

		userID, ok := getUserID(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		title := strings.TrimSpace(r.FormValue("title"))
		description := strings.TrimSpace(r.FormValue("description"))
		category := strings.TrimSpace(r.FormValue("category"))
		privacy := strings.TrimSpace(r.FormValue("privacy"))
		if privacy != "private" {
			privacy = "public"
		}
		privateKey := ""
		if privacy == "private" {
			privateKey = strings.TrimSpace(r.FormValue("private_key"))
		}
		status := strings.TrimSpace(r.FormValue("status"))
		if status != "closed" {
			status = "open"
		}
		if title == "" {
			http.Error(w, "Название квиза не может быть пустым", http.StatusBadRequest)
			return
		}
		if category == "" {
			category = "other"
		}

		timeLimitSeconds, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("time_limit_seconds")))
		if timeLimitSeconds < 0 {
			timeLimitSeconds = 0
		}
		passScore, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("pass_score")))
		if passScore < 0 {
			passScore = 0
		}

		tx, err := db.Begin()
		if err != nil {
			log.Printf("Не удалось начать транзакцию редактирования квиза %d: %v", quizID, err)
			http.Error(w, "Ошибка базы данных", http.StatusInternalServerError)
			return
		}
		defer func() { _ = tx.Rollback() }()

		fail := func(message string, err error) {
			if err != nil {
				log.Printf("%s: %v", message, err)
			} else {
				log.Print(message)
			}
			http.Error(w, "Ошибка базы данных при сохранении квиза", http.StatusInternalServerError)
		}

		_, err = tx.Exec(
			"UPDATE quizzes SET title = ?, description = ?, category = ?, privacy = ?, private_key = ?, status = ?, time_limit_seconds = ?, pass_score = ? WHERE id = ?",
			title, description, category, privacy, privateKey, status, timeLimitSeconds, passScore, quizID,
		)
		if err != nil {
			fail(fmt.Sprintf("Не удалось обновить квиз %d", quizID), err)
			return
		}

		deletedQuestions := map[int]bool{}
		for key, values := range r.PostForm {
			if !strings.HasPrefix(key, "delete_question_") {
				continue
			}
			if len(values) == 0 || strings.TrimSpace(values[0]) != "1" {
				continue
			}
			qid, err := strconv.Atoi(strings.TrimPrefix(key, "delete_question_"))
			if err == nil && qid > 0 {
				deletedQuestions[qid] = true
			}
		}

		for qid := range deletedQuestions {
			_, err = tx.Exec(`
				DELETE qo FROM question_options qo
				INNER JOIN questions q ON q.id = qo.question_id
				WHERE q.id = ? AND q.quiz_id = ?`,
				qid, quizID,
			)
			if err != nil {
				fail(fmt.Sprintf("Не удалось удалить варианты вопроса %d", qid), err)
				return
			}

			_, err = tx.Exec("DELETE FROM questions WHERE id = ? AND quiz_id = ?", qid, quizID)
			if err != nil {
				fail(fmt.Sprintf("Не удалось удалить вопрос %d", qid), err)
				return
			}
		}

		deletedOptions := map[int]bool{}
		for key, values := range r.PostForm {
			if !strings.HasPrefix(key, "delete_option_") {
				continue
			}
			if len(values) == 0 || strings.TrimSpace(values[0]) != "1" {
				continue
			}
			oid, err := strconv.Atoi(strings.TrimPrefix(key, "delete_option_"))
			if err == nil && oid > 0 {
				deletedOptions[oid] = true
			}
		}

		for oid := range deletedOptions {
			_, err = tx.Exec(`
				DELETE qo FROM question_options qo
				INNER JOIN questions q ON q.id = qo.question_id
				WHERE qo.id = ? AND q.quiz_id = ?`,
				oid, quizID,
			)
			if err != nil {
				fail(fmt.Sprintf("Не удалось удалить вариант ответа %d", oid), err)
				return
			}
		}

		existingQuestionIDs := make([]int, 0)
		seenExistingQuestions := make(map[int]struct{})
		for key := range r.PostForm {
			if !strings.HasPrefix(key, "question_text_") {
				continue
			}
			qid, err := strconv.Atoi(strings.TrimPrefix(key, "question_text_"))
			if err != nil || qid <= 0 {
				continue
			}
			if _, exists := seenExistingQuestions[qid]; exists {
				continue
			}
			seenExistingQuestions[qid] = struct{}{}
			existingQuestionIDs = append(existingQuestionIDs, qid)
		}
		sort.Ints(existingQuestionIDs)

		for _, qid := range existingQuestionIDs {
			if deletedQuestions[qid] {
				continue
			}

			var oldType string
			err = tx.QueryRow("SELECT type FROM questions WHERE id = ? AND quiz_id = ?", qid, quizID).Scan(&oldType)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					continue
				}
				fail(fmt.Sprintf("Не удалось прочитать вопрос %d", qid), err)
				return
			}
			oldType = strings.ToLower(strings.TrimSpace(oldType))

			text := strings.TrimSpace(r.FormValue("question_text_" + strconv.Itoa(qid)))
			pointsStr := strings.TrimSpace(r.FormValue("question_points_" + strconv.Itoa(qid)))
			points, _ := strconv.Atoi(pointsStr)
			if points < 0 {
				points = 0
			}

			qType := strings.ToLower(strings.TrimSpace(r.FormValue("question_type_" + strconv.Itoa(qid))))
			if qType == "" {
				qType = oldType
			}
			if qType == "" {
				qType = "single"
			}

			questionMediaURL, questionMediaType := normalizeQuestionMedia(
				r.FormValue("question_media_url_"+strconv.Itoa(qid)),
				r.FormValue("question_media_type_"+strconv.Itoa(qid)),
			)

			_, err = tx.Exec(
				"UPDATE questions SET text = ?, points = ?, type = ?, question_media_url = ?, question_media_type = ? WHERE id = ? AND quiz_id = ?",
				text, points, qType, questionMediaURL, questionMediaType, qid, quizID,
			)
			if err != nil {
				fail(fmt.Sprintf("Не удалось обновить вопрос %d", qid), err)
				return
			}

			if isChoiceQuestionType(qType) {
				prefix := "option_text_" + strconv.Itoa(qid) + "_"
				optionKeys := make([]int, 0)
				seenOptions := make(map[int]struct{})
				for k2 := range r.PostForm {
					if !strings.HasPrefix(k2, prefix) {
						continue
					}
					oid, err := strconv.Atoi(strings.TrimPrefix(k2, prefix))
					if err != nil || oid <= 0 {
						continue
					}
					if _, exists := seenOptions[oid]; exists {
						continue
					}
					seenOptions[oid] = struct{}{}
					optionKeys = append(optionKeys, oid)
				}
				sort.Ints(optionKeys)

				for _, oid := range optionKeys {
					if deletedOptions[oid] {
						continue
					}

					optText := strings.TrimSpace(r.FormValue("option_text_" + strconv.Itoa(qid) + "_" + strconv.Itoa(oid)))
					imgKey := "option_img_" + strconv.Itoa(qid) + "_" + strconv.Itoa(oid)
					optImg := strings.TrimSpace(r.FormValue(imgKey))
					if optText == "" && optImg == "" {
						continue
					}
					correctKey := "option_correct_" + strconv.Itoa(qid) + "_" + strconv.Itoa(oid)
					isCorrect := r.FormValue(correctKey) != ""

					_, err = tx.Exec(`
						UPDATE question_options qo
						INNER JOIN questions q ON q.id = qo.question_id
						SET qo.option_text = ?, qo.option_image_url = ?, qo.is_correct = ?
						WHERE qo.id = ? AND qo.question_id = ? AND q.quiz_id = ?`,
						optText, optImg, isCorrect, oid, qid, quizID,
					)
					if err != nil {
						fail(fmt.Sprintf("Не удалось обновить вариант ответа %d", oid), err)
						return
					}
				}

				cntKey := "existing_new_option_count_" + strconv.Itoa(qid)
				cnt, _ := strconv.Atoi(strings.TrimSpace(r.FormValue(cntKey)))
				if cnt < 0 {
					cnt = 0
				}
				for i := 1; i <= cnt; i++ {
					txtKey := "existing_new_option_text_" + strconv.Itoa(qid) + "_" + strconv.Itoa(i)
					imgKey := "existing_new_option_img_" + strconv.Itoa(qid) + "_" + strconv.Itoa(i)
					cKey := "existing_new_option_correct_" + strconv.Itoa(qid) + "_" + strconv.Itoa(i)

					optText := strings.TrimSpace(r.FormValue(txtKey))
					optImg := strings.TrimSpace(r.FormValue(imgKey))
					if optText == "" && optImg == "" {
						continue
					}
					isCorrect := r.FormValue(cKey) != ""

					_, err = tx.Exec(
						"INSERT INTO question_options (question_id, option_text, option_image_url, is_correct) VALUES (?, ?, ?, ?)",
						qid, optText, optImg, isCorrect,
					)
					if err != nil {
						fail(fmt.Sprintf("Не удалось добавить новый вариант к вопросу %d", qid), err)
						return
					}
				}

				if isSingleChoiceQuestionType(qType) {
					rows, err := tx.Query("SELECT id FROM question_options WHERE question_id = ? AND is_correct = 1 ORDER BY id", qid)
					if err != nil {
						fail(fmt.Sprintf("Не удалось прочитать правильные варианты вопроса %d", qid), err)
						return
					}

					correctIDs := make([]int, 0)
					for rows.Next() {
						var id int
						if err := rows.Scan(&id); err != nil {
							_ = rows.Close()
							fail(fmt.Sprintf("Не удалось обработать правильные варианты вопроса %d", qid), err)
							return
						}
						correctIDs = append(correctIDs, id)
					}
					if err := rows.Err(); err != nil {
						_ = rows.Close()
						fail(fmt.Sprintf("Ошибка чтения правильных вариантов вопроса %d", qid), err)
						return
					}
					_ = rows.Close()

					if len(correctIDs) > 1 {
						keep := correctIDs[0]
						_, err = tx.Exec("UPDATE question_options SET is_correct = 0 WHERE question_id = ? AND id <> ?", qid, keep)
						if err != nil {
							fail(fmt.Sprintf("Не удалось нормализовать правильный ответ вопроса %d", qid), err)
							return
						}
					}
				}
			} else if isOpenQuestionType(qType) {
				openKey := "open_answer_" + strconv.Itoa(qid)
				if _, hasOpenAnswersField := r.PostForm[openKey]; hasOpenAnswersField {
					_, err = tx.Exec(`
						DELETE qo FROM question_options qo
						INNER JOIN questions q ON q.id = qo.question_id
						WHERE q.id = ? AND q.quiz_id = ?`,
						qid, quizID,
					)
					if err != nil {
						fail(fmt.Sprintf("Не удалось очистить ответы открытого вопроса %d", qid), err)
						return
					}

					openAnswersSeen := make(map[string]struct{})
					for _, line := range strings.Split(r.FormValue(openKey), "\n") {
						answerText := strings.TrimSpace(line)
						if answerText == "" {
							continue
						}
						normalized := normalizeOpenAnswer(answerText)
						if normalized == "" {
							continue
						}
						if _, exists := openAnswersSeen[normalized]; exists {
							continue
						}
						openAnswersSeen[normalized] = struct{}{}

						_, err = tx.Exec(
							"INSERT INTO question_options (question_id, option_text, option_image_url, is_correct) VALUES (?, ?, ?, ?)",
							qid, answerText, "", true,
						)
						if err != nil {
							fail(fmt.Sprintf("Не удалось добавить ответ открытого вопроса %d", qid), err)
							return
						}
					}
				} else if oldType != "open" {
					_, err = tx.Exec(`
						DELETE qo FROM question_options qo
						INNER JOIN questions q ON q.id = qo.question_id
						WHERE q.id = ? AND q.quiz_id = ?`,
						qid, quizID,
					)
					if err != nil {
						fail(fmt.Sprintf("Не удалось удалить старые варианты вопроса %d", qid), err)
						return
					}
				}
			} else {
				_, err = tx.Exec(`
					DELETE qo FROM question_options qo
					INNER JOIN questions q ON q.id = qo.question_id
					WHERE q.id = ? AND q.quiz_id = ?`,
					qid, quizID,
				)
				if err != nil {
					fail(fmt.Sprintf("Не удалось удалить варианты вопроса %d", qid), err)
					return
				}
			}
		}

		newTotalStr := strings.TrimSpace(r.FormValue("new_total_questions"))
		newTotal, _ := strconv.Atoi(newTotalStr)
		if newTotal < 0 {
			newTotal = 0
		}

		for i := 1; i <= newTotal; i++ {
			qText := strings.TrimSpace(r.FormValue(fmt.Sprintf("new_question_text_%d", i)))
			qType := strings.ToLower(strings.TrimSpace(r.FormValue(fmt.Sprintf("new_question_type_%d", i))))
			if qType == "" {
				qType = "single"
			}
			if qText == "" && qType != "video" {
				continue
			}

			qPointsStr := strings.TrimSpace(r.FormValue(fmt.Sprintf("new_question_points_%d", i)))
			qPoints, _ := strconv.Atoi(qPointsStr)
			if qPoints < 0 {
				qPoints = 0
			}

			qMediaURL, qMediaType := normalizeQuestionMedia(
				r.FormValue(fmt.Sprintf("new_question_media_url_%d", i)),
				r.FormValue(fmt.Sprintf("new_question_media_type_%d", i)),
			)

			res, err := tx.Exec(
				"INSERT INTO questions (text, type, points, author_id, quiz_id, question_media_url, question_media_type) VALUES (?, ?, ?, ?, ?, ?, ?)",
				qText, qType, qPoints, userID, quizID, qMediaURL, qMediaType,
			)
			if err != nil {
				fail("Не получилось вставить новый вопрос при редактировании", err)
				return
			}

			newQuestionID, err := res.LastInsertId()
			if err != nil {
				fail("Не удалось получить ID нового вопроса при редактировании", err)
				return
			}

			if isOpenQuestionType(qType) {
				openAnswersSeen := make(map[string]struct{})
				for _, line := range strings.Split(r.FormValue(fmt.Sprintf("new_open_answer_%d", i)), "\n") {
					answerText := strings.TrimSpace(line)
					if answerText == "" {
						continue
					}
					normalized := normalizeOpenAnswer(answerText)
					if normalized == "" {
						continue
					}
					if _, exists := openAnswersSeen[normalized]; exists {
						continue
					}
					openAnswersSeen[normalized] = struct{}{}

					_, err = tx.Exec(
						"INSERT INTO question_options (question_id, option_text, option_image_url, is_correct) VALUES (?, ?, ?, ?)",
						newQuestionID, answerText, "", true,
					)
					if err != nil {
						fail("Не получилось вставить ответ нового открытого вопроса", err)
						return
					}
				}
				continue
			}

			if !isChoiceQuestionType(qType) {
				continue
			}

			optCountStr := strings.TrimSpace(r.FormValue(fmt.Sprintf("new_option_count_%d", i)))
			optCount, _ := strconv.Atoi(optCountStr)
			if optCount < 0 {
				optCount = 0
			}

			type optionInput struct {
				text      string
				imageURL  string
				isCorrect bool
			}

			options := make([]optionInput, 0, optCount)
			for j := 1; j <= optCount; j++ {
				optText := strings.TrimSpace(r.FormValue(fmt.Sprintf("new_option_%d_%d", i, j)))
				optImg := strings.TrimSpace(r.FormValue(fmt.Sprintf("new_option_img_%d_%d", i, j)))
				if optText == "" && optImg == "" {
					continue
				}
				options = append(options, optionInput{
					text:      optText,
					imageURL:  optImg,
					isCorrect: r.FormValue(fmt.Sprintf("new_is_correct_%d_%d", i, j)) != "",
				})
			}

			if isSingleChoiceQuestionType(qType) && len(options) > 0 {
				hasCorrect := false
				for idx := range options {
					if !options[idx].isCorrect {
						continue
					}
					if hasCorrect {
						options[idx].isCorrect = false
						continue
					}
					hasCorrect = true
				}
				if !hasCorrect {
					options[0].isCorrect = true
				}
			}

			for _, opt := range options {
				_, err = tx.Exec(
					"INSERT INTO question_options (question_id, option_text, option_image_url, is_correct) VALUES (?, ?, ?, ?)",
					newQuestionID, opt.text, opt.imageURL, opt.isCorrect,
				)
				if err != nil {
					fail("Не получилось вставить вариант нового вопроса при редактировании", err)
					return
				}
			}
		}

		if err := tx.Commit(); err != nil {
			fail(fmt.Sprintf("Не удалось завершить транзакцию редактирования квиза %d", quizID), err)
			return
		}

		http.Redirect(w, r, "/edit-quiz/"+strconv.Itoa(quizID), http.StatusSeeOther)
		return
	}

	if r.Method != http.MethodGet {
		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		return
	}

	var q quiz
	err = db.QueryRow("SELECT "+quizSelectColumns+" FROM quizzes WHERE id = ?", quizID).Scan(
		&q.Id, &q.Title, &q.Description, &q.Category, &q.Author, &q.PrivateKey, &q.Privacy, &q.Status, &q.TimeLimitSeconds, &q.PassScore, &q.ParticipationMode, &q.TeamCount,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		log.Printf("Ошибка чтения квиза %d: %v", quizID, err)
		http.Error(w, "Ошибка базы данных", http.StatusInternalServerError)
		return
	}
	q.ParticipationMode = normalizeParticipationMode(q.ParticipationMode)
	q.TeamCount = normalizeTeamCount(q.ParticipationMode, q.TeamCount)

	rows, err := db.Query("SELECT id, text, type, points, COALESCE(question_media_url, ''), COALESCE(question_media_type, '') FROM questions WHERE quiz_id = ? ORDER BY id", quizID)
	if err != nil {
		log.Printf("Ошибка загрузки вопросов квиза %d: %v", quizID, err)
		http.Error(w, "Ошибка базы данных", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var questions []QuestionWithOptions
	for rows.Next() {
		var qs question
		var questionMediaURL, questionMediaType string
		if err := rows.Scan(&qs.Id, &qs.Text, &qs.Type, &qs.Points, &questionMediaURL, &questionMediaType); err != nil {
			log.Printf("Ошибка чтения вопроса квиза %d: %v", quizID, err)
			http.Error(w, "Ошибка базы данных", http.StatusInternalServerError)
			return
		}
		qs.Type = strings.ToLower(strings.TrimSpace(qs.Type))
		qs.QuestionMediaURL, qs.QuestionMediaType = normalizeQuestionMedia(questionMediaURL, questionMediaType)

		item := QuestionWithOptions{Question: qs}
		if isChoiceQuestionType(qs.Type) || isOpenQuestionType(qs.Type) {
			optRows, err := db.Query("SELECT id, option_text, option_image_url, is_correct FROM question_options WHERE question_id = ? ORDER BY id", qs.Id)
			if err != nil {
				log.Printf("Ошибка загрузки вариантов вопроса %d: %v", qs.Id, err)
				http.Error(w, "Ошибка базы данных", http.StatusInternalServerError)
				return
			}

			accepted := make([]string, 0, 4)
			acceptedSeen := make(map[string]struct{}, 4)
			for optRows.Next() {
				var (
					opt struct {
						Id         int
						OptionText string
						ImageURL   string
						IsCorrect  bool
					}
					img sql.NullString
				)
				if err := optRows.Scan(&opt.Id, &opt.OptionText, &img, &opt.IsCorrect); err != nil {
					_ = optRows.Close()
					log.Printf("Ошибка чтения варианта вопроса %d: %v", qs.Id, err)
					http.Error(w, "Ошибка базы данных", http.StatusInternalServerError)
					return
				}
				opt.ImageURL = nullStringToString(img)
				opt.OptionText = strings.TrimSpace(opt.OptionText)
				if isChoiceQuestionType(qs.Type) {
					item.Options = append(item.Options, opt)
				}
				if isOpenQuestionType(qs.Type) && opt.IsCorrect && opt.OptionText != "" {
					norm := normalizeOpenAnswer(opt.OptionText)
					if norm != "" {
						if _, exists := acceptedSeen[norm]; !exists {
							acceptedSeen[norm] = struct{}{}
							accepted = append(accepted, opt.OptionText)
						}
					}
				}
			}
			if err := optRows.Err(); err != nil {
				_ = optRows.Close()
				log.Printf("Ошибка чтения вариантов вопроса %d: %v", qs.Id, err)
				http.Error(w, "Ошибка базы данных", http.StatusInternalServerError)
				return
			}
			_ = optRows.Close()

			if len(accepted) > 0 {
				item.OpenAnswersText = strings.Join(accepted, "\n")
			}
		}

		questions = append(questions, item)
	}
	if err := rows.Err(); err != nil {
		log.Printf("Ошибка чтения вопросов квиза %d: %v", quizID, err)
		http.Error(w, "Ошибка базы данных", http.StatusInternalServerError)
		return
	}

	data := editQuizPageData{
		Username:      username,
		Quiz:          q,
		Questions:     questions,
		ExistingCount: len(questions),
		IsNew:         false,
	}

	renderEditQuizTemplate(w, data)
}

func handleDeleteQuiz(w http.ResponseWriter, r *http.Request) {
	username, _ := getCurrentUsername(r)
	quizID, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/delete-quiz/"))
	var author string
	db.QueryRow("SELECT author FROM quizzes WHERE id = ?", quizID).Scan(&author)
	if username == author && r.Method == "POST" {
		db.Exec("DELETE FROM question_options WHERE question_id IN (SELECT id FROM questions WHERE quiz_id = ?)", quizID)
		db.Exec("DELETE FROM questions WHERE quiz_id = ?", quizID)
		db.Exec("DELETE FROM quizzes WHERE id = ?", quizID)
		http.Redirect(w, r, "/my-quizzes/"+author, http.StatusSeeOther)
	} else {
		fmt.Fprintf(w, "Страница не найдена")
	}
}

func (e quizStatsLoadError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return e.Message
}

func handleQuizStats(w http.ResponseWriter, r *http.Request) {
	if !isAuthenticated(r) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	currentUsername, _ := getCurrentUsername(r)

	path := strings.TrimPrefix(r.URL.Path, "/quiz-stats/")
	path = strings.Trim(path, "/")
	if path == "" {
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(path, "/")

	quizID, err := strconv.Atoi(parts[0])
	if err != nil || quizID <= 0 {
		http.Error(w, "Некорректный quiz_id", http.StatusBadRequest)
		return
	}

	if len(parts) >= 2 && parts[1] == "export-excel" {
		exportQuizStatsExcel(w, r, currentUsername, quizID)
		return
	}

	if len(parts) >= 3 && parts[1] == "participant" {
		sessionID, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil || sessionID <= 0 {
			http.Error(w, "Некорректный session_id", http.StatusBadRequest)
			return
		}
		renderQuizParticipantStats(w, r, currentUsername, quizID, sessionID)
		return
	}

	renderQuizStats(w, r, currentUsername, quizID)
}

func quizQuestionTypeLabel(t string) string {
	switch t {
	case "single":
		return "Одиночный выбор"
	case "multiple":
		return "Множественный выбор"
	case "photo_single":
		return "Фото-одиночный выбор"
	case "photo_multiple":
		return "Фото-множественный выбор"
	case "open":
		return "Открытый вопрос"
	case "photo":
		return "Фото"
	case "video":
		return "Видео"
	default:
		return t
	}
}

func loadQuizStatsReport(currentUsername string, quizID int, attemptsLimit int, includeAnswers bool) (quizStatsReportData, error) {
	data := quizStatsReportData{Username: currentUsername}

	var q quiz
	err := db.QueryRow("SELECT "+quizSelectColumns+" FROM quizzes WHERE id = ?", quizID).Scan(&q.Id, &q.Title, &q.Description, &q.Category, &q.Author, &q.PrivateKey, &q.Privacy, &q.Status, &q.TimeLimitSeconds, &q.PassScore, &q.ParticipationMode, &q.TeamCount)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return data, quizStatsLoadError{Status: http.StatusNotFound, Message: "Квиз не найден", Err: err}
		}
		return data, quizStatsLoadError{Status: http.StatusInternalServerError, Message: "Ошибка базы данных", Err: err}
	}
	if q.Author != currentUsername {
		return data, quizStatsLoadError{Status: http.StatusForbidden, Message: "Доступ запрещён", Err: errors.New("access denied")}
	}
	data.Quiz = q

	summary := quizStatsSummary{QuizID: quizID, Title: q.Title}
	if err := db.QueryRow(
		`SELECT
			COUNT(*) AS attempts_total,
			COUNT(DISTINCT CASE
				WHEN COALESCE(NULLIF(TRIM(participant_name), ''), '') <> '' THEN CONCAT('name:', LOWER(TRIM(participant_name)))
				WHEN user_id > 0 THEN CONCAT('uid:', user_id)
				ELSE CONCAT('session:', id)
			END) AS participants_total,
			COALESCE(AVG(score), 0) AS avg_score,
			COALESCE(AVG(max_score), 0) AS avg_max
		FROM quiz_sessions
		WHERE quiz_id = ?`, quizID,
	).Scan(&summary.AttemptsTotal, &summary.ParticipantsTotal, &summary.AvgScore, &summary.AvgMaxScore); err != nil {
		return data, quizStatsLoadError{Status: http.StatusInternalServerError, Message: "Ошибка базы данных", Err: err}
	}

	var lastStarted, lastFinished sql.NullTime
	_ = db.QueryRow(
		`SELECT started_at, finished_at
		FROM quiz_sessions
		WHERE quiz_id = ?
		ORDER BY finished_at DESC, id DESC
		LIMIT 1`, quizID,
	).Scan(&lastStarted, &lastFinished)
	if lastStarted.Valid {
		summary.LastStartedAt = lastStarted.Time.Format("2006-01-02 15:04:05")
	}
	if lastFinished.Valid {
		summary.LastFinishedAt = lastFinished.Time.Format("2006-01-02 15:04:05")
	}

	attemptsLimitClause := ""
	if attemptsLimit > 0 {
		attemptsLimitClause = fmt.Sprintf(" LIMIT %d", attemptsLimit)
	}
	rows, err := db.Query(
		`SELECT 
			qs.id,
			qs.user_id,
			COALESCE(NULLIF(TRIM(qs.participant_name), ''), 'Участник') AS participant_name,
			COALESCE(NULLIF(TRIM(qs.team_name), ''), '') AS team_name,
			qs.attempt_number,
			qs.started_at,
			qs.finished_at,
			qs.score,
			qs.max_score
		FROM quiz_sessions qs
		LEFT JOIN users u ON u.id = qs.user_id
		WHERE qs.quiz_id = ?
		ORDER BY qs.finished_at DESC, qs.id DESC`+attemptsLimitClause, quizID,
	)
	if err != nil {
		return data, quizStatsLoadError{Status: http.StatusInternalServerError, Message: "Ошибка базы данных", Err: err}
	}
	defer rows.Close()

	participants := make([]quizStatsParticipantRow, 0, 64)
	for rows.Next() {
		var pr quizStatsParticipantRow
		var userIDValue sql.NullInt64
		var st, ft time.Time
		if err := rows.Scan(&pr.SessionID, &userIDValue, &pr.ParticipantName, &pr.TeamName, &pr.AttemptNumber, &st, &ft, &pr.Score, &pr.MaxScore); err != nil {
			return data, quizStatsLoadError{Status: http.StatusInternalServerError, Message: "Ошибка базы данных", Err: err}
		}
		if userIDValue.Valid {
			pr.UserID = int(userIDValue.Int64)
		}
		pr.StartedAt = st.Format("2006-01-02 15:04:05")
		pr.FinishedAt = ft.Format("2006-01-02 15:04:05")
		if ft.Before(st) {
			ft = st
		}
		pr.DurationSecond = int(ft.Sub(st).Seconds())
		pr.DurationHuman = formatDurationHuman(ft.Sub(st))
		participants = append(participants, pr)
	}
	if err := rows.Err(); err != nil {
		return data, quizStatsLoadError{Status: http.StatusInternalServerError, Message: "Ошибка базы данных", Err: err}
	}
	data.Participants = participants

	if isTeamMode(q.ParticipationMode) {
		standings, _, err := buildQuizTeamLeaderboard(quizID, "", isTeamRouletteMode(q.ParticipationMode))
		if err != nil {
			return data, quizStatsLoadError{Status: http.StatusInternalServerError, Message: "Ошибка базы данных", Err: err}
		}
		summary.TeamsTotal = len(standings)
		data.TeamLeaderboard = standings
	}

	qRows, err := db.Query("SELECT id, text, type, points, author_id, quiz_id, COALESCE(question_media_url, ''), COALESCE(question_media_type, '') FROM questions WHERE quiz_id = ? ORDER BY id", quizID)
	if err != nil {
		return data, quizStatsLoadError{Status: http.StatusInternalServerError, Message: "Ошибка базы данных", Err: err}
	}
	defer qRows.Close()

	questions := make([]quizStatsQuestion, 0, 32)
	idx := 0
	for qRows.Next() {
		var qs question
		var questionMediaURL, questionMediaType string
		if err := qRows.Scan(&qs.Id, &qs.Text, &qs.Type, &qs.Points, &qs.AuthorID, &qs.QuizID, &questionMediaURL, &questionMediaType); err != nil {
			return data, quizStatsLoadError{Status: http.StatusInternalServerError, Message: "Ошибка базы данных", Err: err}
		}
		qs.QuestionMediaURL, qs.QuestionMediaType = normalizeQuestionMedia(questionMediaURL, questionMediaType)
		idx++
		qq := quizStatsQuestion{
			Index:             idx,
			QuestionID:        qs.Id,
			Text:              qs.Text,
			Type:              qs.Type,
			Points:            qs.Points,
			QuestionMediaURL:  qs.QuestionMediaURL,
			QuestionMediaType: qs.QuestionMediaType,
		}

		if err := db.QueryRow(
			`SELECT COUNT(DISTINCT ua.session_id)
			FROM user_answers ua
			JOIN quiz_sessions s ON s.id = ua.session_id
			WHERE s.quiz_id = ? AND ua.question_id = ?`, quizID, qs.Id,
		).Scan(&qq.Total); err != nil {
			return data, quizStatsLoadError{Status: http.StatusInternalServerError, Message: "Ошибка базы данных", Err: err}
		}

		if err := db.QueryRow(
			`SELECT COUNT(DISTINCT ua.session_id)
			FROM user_answers ua
			JOIN quiz_sessions s ON s.id = ua.session_id
			WHERE s.quiz_id = ? AND ua.question_id = ? AND ua.is_correct = 1`, quizID, qs.Id,
		).Scan(&qq.Correct); err != nil {
			return data, quizStatsLoadError{Status: http.StatusInternalServerError, Message: "Ошибка базы данных", Err: err}
		}

		var hasAuto int
		if err := db.QueryRow(
			`SELECT CASE WHEN COUNT(*) > 0 THEN 1 ELSE 0 END
			FROM user_answers ua
			JOIN quiz_sessions s ON s.id = ua.session_id
			WHERE s.quiz_id = ? AND ua.question_id = ? AND ua.is_correct IS NOT NULL`, quizID, qs.Id,
		).Scan(&hasAuto); err != nil {
			return data, quizStatsLoadError{Status: http.StatusInternalServerError, Message: "Ошибка базы данных", Err: err}
		}
		qq.HasAutoCheck = hasAuto == 1

		if qq.Total > 0 && qq.HasAutoCheck {
			qq.CorrectPct = float64(qq.Correct) * 100.0 / float64(qq.Total)
		}

		optRows, err := db.Query("SELECT id, option_text, option_image_url, is_correct FROM question_options WHERE question_id = ? ORDER BY id", qs.Id)
		if err != nil {
			return data, quizStatsLoadError{Status: http.StatusInternalServerError, Message: "Ошибка базы данных", Err: err}
		}
		options := make([]quizStatsOptionDist, 0, 8)
		acceptedSeen := make(map[string]struct{}, 4)
		for optRows.Next() {
			var oid int
			var text string
			var img sql.NullString
			var isCorr bool
			if err := optRows.Scan(&oid, &text, &img, &isCorr); err != nil {
				_ = optRows.Close()
				return data, quizStatsLoadError{Status: http.StatusInternalServerError, Message: "Ошибка базы данных", Err: err}
			}
			cleanText := strings.TrimSpace(text)
			options = append(options, quizStatsOptionDist{OptionID: oid, OptionText: cleanText, OptionImageURL: nullStringToString(img), IsCorrect: isCorr})
			if isOpenQuestionType(qs.Type) && isCorr && cleanText != "" {
				norm := normalizeOpenAnswer(cleanText)
				if norm != "" {
					if _, exists := acceptedSeen[norm]; !exists {
						acceptedSeen[norm] = struct{}{}
						qq.AcceptedAnswers = append(qq.AcceptedAnswers, cleanText)
					}
				}
			}
		}
		if err := optRows.Err(); err != nil {
			_ = optRows.Close()
			return data, quizStatsLoadError{Status: http.StatusInternalServerError, Message: "Ошибка базы данных", Err: err}
		}
		_ = optRows.Close()

		counts := make(map[int]int, len(options))
		skipped := 0
		distRows, err := db.Query(
			`SELECT ua.selected_option_id, COUNT(*)
			FROM user_answers ua
			JOIN quiz_sessions s ON s.id = ua.session_id
			WHERE s.quiz_id = ? AND ua.question_id = ?
			GROUP BY ua.selected_option_id`, quizID, qs.Id,
		)
		if err != nil {
			return data, quizStatsLoadError{Status: http.StatusInternalServerError, Message: "Ошибка базы данных", Err: err}
		}
		for distRows.Next() {
			var optID sql.NullInt64
			var c int
			if err := distRows.Scan(&optID, &c); err != nil {
				_ = distRows.Close()
				return data, quizStatsLoadError{Status: http.StatusInternalServerError, Message: "Ошибка базы данных", Err: err}
			}
			if !optID.Valid {
				skipped += c
				continue
			}
			counts[int(optID.Int64)] = c
		}
		if err := distRows.Err(); err != nil {
			_ = distRows.Close()
			return data, quizStatsLoadError{Status: http.StatusInternalServerError, Message: "Ошибка базы данных", Err: err}
		}
		_ = distRows.Close()

		totalSelections := 0
		for _, c := range counts {
			totalSelections += c
		}
		totalWithSkip := totalSelections + skipped

		qq.SkippedCount = skipped
		if totalWithSkip > 0 {
			qq.SkippedPct = float64(skipped) * 100.0 / float64(totalWithSkip)
		}

		for i := range options {
			options[i].Count = counts[options[i].OptionID]
			if totalWithSkip > 0 {
				options[i].Percent = float64(options[i].Count) * 100.0 / float64(totalWithSkip)
			}
		}
		qq.Options = options

		questions = append(questions, qq)
	}
	if err := qRows.Err(); err != nil {
		return data, quizStatsLoadError{Status: http.StatusInternalServerError, Message: "Ошибка базы данных", Err: err}
	}
	data.Questions = questions
	data.Summary = summary

	if includeAnswers {
		answers, err := loadQuizStatsAnswers(quizID, questions)
		if err != nil {
			return data, quizStatsLoadError{Status: http.StatusInternalServerError, Message: "Ошибка базы данных", Err: err}
		}
		data.Answers = answers
	}

	return data, nil
}

func loadQuizStatsAnswers(quizID int, questions []quizStatsQuestion) ([]quizStatsAnswerRow, error) {
	questionIndexByID := make(map[int]int, len(questions))
	for _, q := range questions {
		questionIndexByID[q.QuestionID] = q.Index
	}

	rows, err := db.Query(
		`SELECT
			qs.id,
			COALESCE(NULLIF(TRIM(qs.participant_name), ''), 'Участник') AS participant_name,
			COALESCE(NULLIF(TRIM(qs.team_name), ''), '') AS team_name,
			qs.attempt_number,
			q.id,
			q.text,
			q.type,
			ua.selected_option_id,
			COALESCE(qo.option_text, '') AS selected_option_text,
			COALESCE(ua.answer_text, '') AS answer_text,
			ua.is_correct
		FROM user_answers ua
		JOIN quiz_sessions qs ON qs.id = ua.session_id
		JOIN questions q ON q.id = ua.question_id
		LEFT JOIN question_options qo ON qo.id = ua.selected_option_id
		WHERE qs.quiz_id = ?
		ORDER BY qs.finished_at DESC, qs.id DESC, q.id, ua.selected_option_id`, quizID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	answers := make([]quizStatsAnswerRow, 0, 256)
	for rows.Next() {
		var a quizStatsAnswerRow
		var selectedOptionID sql.NullInt64
		var isCorrect sql.NullBool
		if err := rows.Scan(&a.SessionID, &a.ParticipantName, &a.TeamName, &a.AttemptNumber, &a.QuestionID, &a.QuestionText, &a.QuestionType, &selectedOptionID, &a.SelectedOptionText, &a.AnswerText, &isCorrect); err != nil {
			return nil, err
		}
		if selectedOptionID.Valid {
			a.SelectedOptionID = int(selectedOptionID.Int64)
		}
		if isCorrect.Valid {
			a.IsCorrectKnown = true
			a.IsCorrect = isCorrect.Bool
		}
		a.QuestionIndex = questionIndexByID[a.QuestionID]
		answers = append(answers, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return answers, nil
}

func renderQuizStats(w http.ResponseWriter, r *http.Request, currentUsername string, quizID int) {
	data, err := loadQuizStatsReport(currentUsername, quizID, 500, false)
	if err != nil {
		status := http.StatusInternalServerError
		message := "Ошибка базы данных"
		if e, ok := err.(quizStatsLoadError); ok {
			status = e.Status
			message = e.Message
		}
		if status >= http.StatusInternalServerError {
			log.Printf("Ошибка формирования статистики квиза %d: %v", quizID, err)
		}
		http.Error(w, message, status)
		return
	}

	tmpl, err := template.ParseFiles("frontend/quizStats.html")
	if err != nil {
		log.Printf("Не найден шаблон quizStats.html: %v", err)
		http.Error(w, "Не найден шаблон quizStats.html", http.StatusInternalServerError)
		return
	}
	if err := tmpl.Execute(w, data); err != nil {
		log.Printf("Ошибка рендера quizStats.html для квиза %d: %v", quizID, err)
	}
}

func exportQuizStatsExcel(w http.ResponseWriter, r *http.Request, currentUsername string, quizID int) {
	if r.Method != http.MethodGet {
		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		return
	}

	data, err := loadQuizStatsReport(currentUsername, quizID, 0, true)
	if err != nil {
		status := http.StatusInternalServerError
		message := "Ошибка базы данных"
		if e, ok := err.(quizStatsLoadError); ok {
			status = e.Status
			message = e.Message
		}
		if status >= http.StatusInternalServerError {
			log.Printf("Ошибка подготовки Excel-отчёта квиза %d: %v", quizID, err)
		}
		http.Error(w, message, status)
		return
	}

	fileBytes, err := buildQuizStatsXLSX(data)
	if err != nil {
		log.Printf("Ошибка генерации Excel-отчёта квиза %d: %v", quizID, err)
		http.Error(w, "Не удалось сформировать Excel-отчёт", http.StatusInternalServerError)
		return
	}

	filename := fmt.Sprintf("quiz_%d_report.xlsx", quizID)
	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	w.Header().Set("Content-Length", strconv.Itoa(len(fileBytes)))
	_, _ = w.Write(fileBytes)
}

func xlsxHeader(value string) xlsxCell {
	return xlsxCell{Value: value, Style: 1}
}

func xlsxTitle(value string) xlsxCell {
	return xlsxCell{Value: value, Style: 2}
}

func xlsxText(value string) xlsxCell {
	return xlsxCell{Value: value, Style: 4}
}

func xlsxNumber(value any) xlsxCell {
	return xlsxCell{Value: value, Style: 4}
}

func xlsxPercent(value float64) xlsxCell {
	return xlsxCell{Value: value, Style: 3}
}

func xlsxBlank() xlsxCell {
	return xlsxCell{Style: 4}
}

func xlsxPercentFromParts(value, total int) xlsxCell {
	if total <= 0 {
		return xlsxBlank()
	}
	return xlsxPercent(float64(value) / float64(total))
}

func buildQuizStatsXLSX(data quizStatsReportData) ([]byte, error) {
	sheets := buildQuizStatsXLSSheets(data)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	if err := addXLSXPart(zw, "[Content_Types].xml", xlsxContentTypesXML(len(sheets))); err != nil {
		return nil, err
	}
	if err := addXLSXPart(zw, "_rels/.rels", xlsxRootRelsXML()); err != nil {
		return nil, err
	}
	if err := addXLSXPart(zw, "xl/workbook.xml", xlsxWorkbookXML(sheets)); err != nil {
		return nil, err
	}
	if err := addXLSXPart(zw, "xl/_rels/workbook.xml.rels", xlsxWorkbookRelsXML(len(sheets))); err != nil {
		return nil, err
	}
	if err := addXLSXPart(zw, "xl/styles.xml", xlsxStylesXML()); err != nil {
		return nil, err
	}
	for i, sheet := range sheets {
		name := fmt.Sprintf("xl/worksheets/sheet%d.xml", i+1)
		if err := addXLSXPart(zw, name, xlsxWorksheetXML(sheet)); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func buildQuizStatsXLSSheets(data quizStatsReportData) []xlsxSheet {
	generatedAt := time.Now().Format("2006-01-02 15:04:05")
	passScore := "—"
	if data.Quiz.PassScore > 0 {
		passScore = strconv.Itoa(data.Quiz.PassScore)
	}

	summaryRows := [][]xlsxCell{
		{xlsxTitle("Отчёт по квизу"), xlsxText(data.Quiz.Title)},
		{},
		{xlsxHeader("Показатель"), xlsxHeader("Значение")},
		{xlsxText("ID квиза"), xlsxNumber(data.Quiz.Id)},
		{xlsxText("Название"), xlsxText(data.Quiz.Title)},
		{xlsxText("Автор"), xlsxText(data.Quiz.Author)},
		{xlsxText("Категория"), xlsxText(data.Quiz.Category)},
		{xlsxText("Режим участия"), xlsxText(participationModeLabel(data.Quiz.ParticipationMode))},
		{xlsxText("Статус квиза"), xlsxText(data.Quiz.Status)},
		{xlsxText("Проходной балл"), xlsxText(passScore)},
		{xlsxText("Всего попыток"), xlsxNumber(data.Summary.AttemptsTotal)},
		{xlsxText("Уникальных участников"), xlsxNumber(data.Summary.ParticipantsTotal)},
		{xlsxText("Средний результат"), xlsxNumber(roundFloat(data.Summary.AvgScore, 1))},
		{xlsxText("Средний максимум"), xlsxNumber(roundFloat(data.Summary.AvgMaxScore, 1))},
		{xlsxText("Команд"), xlsxNumber(data.Summary.TeamsTotal)},
		{xlsxText("Последняя попытка"), xlsxText(nonEmptyDash(data.Summary.LastFinishedAt))},
		{xlsxText("Отчёт сформирован"), xlsxText(generatedAt)},
	}

	attemptRows := [][]xlsxCell{{
		xlsxHeader("№"),
		xlsxHeader("Session ID"),
		xlsxHeader("Участник"),
		xlsxHeader("Команда"),
		xlsxHeader("Попытка"),
		xlsxHeader("Начало"),
		xlsxHeader("Завершение"),
		xlsxHeader("Длительность"),
		xlsxHeader("Баллы"),
		xlsxHeader("Максимум"),
		xlsxHeader("Процент"),
		xlsxHeader("Статус"),
	}}
	for i, p := range data.Participants {
		status := "—"
		if data.Quiz.PassScore > 0 {
			if p.Score >= data.Quiz.PassScore {
				status = "Пройден"
			} else {
				status = "Не пройден"
			}
		}
		attemptRows = append(attemptRows, []xlsxCell{
			xlsxNumber(i + 1),
			xlsxNumber(p.SessionID),
			xlsxText(nonEmptyDash(p.ParticipantName)),
			xlsxText(nonEmptyDash(p.TeamName)),
			xlsxNumber(p.AttemptNumber),
			xlsxText(p.StartedAt),
			xlsxText(p.FinishedAt),
			xlsxText(p.DurationHuman),
			xlsxNumber(p.Score),
			xlsxNumber(p.MaxScore),
			xlsxPercentFromParts(p.Score, p.MaxScore),
			xlsxText(status),
		})
	}

	correctAnswers := correctAnswersByQuestionID(data.Questions)
	answerRows := [][]xlsxCell{{
		xlsxHeader("Session ID"),
		xlsxHeader("Участник"),
		xlsxHeader("Команда"),
		xlsxHeader("Попытка"),
		xlsxHeader("№ вопроса"),
		xlsxHeader("Question ID"),
		xlsxHeader("Вопрос"),
		xlsxHeader("Тип"),
		xlsxHeader("Option ID"),
		xlsxHeader("Ответ участника"),
		xlsxHeader("Правильный ответ"),
		xlsxHeader("Оценка"),
	}}
	for _, a := range data.Answers {
		answerText := strings.TrimSpace(a.AnswerText)
		if answerText == "" {
			answerText = strings.TrimSpace(a.SelectedOptionText)
		}
		selectedOptionCell := xlsxBlank()
		if a.SelectedOptionID > 0 {
			selectedOptionCell = xlsxNumber(a.SelectedOptionID)
		}
		status := "Не проверяется"
		if a.IsCorrectKnown {
			if a.IsCorrect {
				status = "Верно"
			} else {
				status = "Неверно"
			}
		}
		answerRows = append(answerRows, []xlsxCell{
			xlsxNumber(a.SessionID),
			xlsxText(nonEmptyDash(a.ParticipantName)),
			xlsxText(nonEmptyDash(a.TeamName)),
			xlsxNumber(a.AttemptNumber),
			xlsxNumber(a.QuestionIndex),
			xlsxNumber(a.QuestionID),
			xlsxText(a.QuestionText),
			xlsxText(quizQuestionTypeLabel(a.QuestionType)),
			selectedOptionCell,
			xlsxText(nonEmptyDash(answerText)),
			xlsxText(nonEmptyDash(correctAnswers[a.QuestionID])),
			xlsxText(status),
		})
	}

	questionRows := [][]xlsxCell{{
		xlsxHeader("№"),
		xlsxHeader("Question ID"),
		xlsxHeader("Вопрос"),
		xlsxHeader("Тип"),
		xlsxHeader("Баллы"),
		xlsxHeader("Ответов"),
		xlsxHeader("Верных"),
		xlsxHeader("Верно, %"),
		xlsxHeader("Автопроверка"),
		xlsxHeader("Пропусков"),
		xlsxHeader("Пропуски, %"),
	}}
	for _, q := range data.Questions {
		correctPct := xlsxBlank()
		if q.HasAutoCheck && q.Total > 0 {
			correctPct = xlsxPercent(q.CorrectPct / 100.0)
		}
		skippedPct := xlsxBlank()
		if q.SkippedPct > 0 {
			skippedPct = xlsxPercent(q.SkippedPct / 100.0)
		}
		autoCheck := "Нет"
		if q.HasAutoCheck {
			autoCheck = "Да"
		}
		questionRows = append(questionRows, []xlsxCell{
			xlsxNumber(q.Index),
			xlsxNumber(q.QuestionID),
			xlsxText(q.Text),
			xlsxText(quizQuestionTypeLabel(q.Type)),
			xlsxNumber(q.Points),
			xlsxNumber(q.Total),
			xlsxNumber(q.Correct),
			correctPct,
			xlsxText(autoCheck),
			xlsxNumber(q.SkippedCount),
			skippedPct,
		})
	}

	distributionRows := [][]xlsxCell{{
		xlsxHeader("№ вопроса"),
		xlsxHeader("Question ID"),
		xlsxHeader("Вопрос"),
		xlsxHeader("Option ID"),
		xlsxHeader("Вариант / значение"),
		xlsxHeader("Правильный"),
		xlsxHeader("Выбрано"),
		xlsxHeader("Процент"),
	}}
	for _, q := range data.Questions {
		for _, opt := range q.Options {
			isCorrect := "Нет"
			if opt.IsCorrect {
				isCorrect = "Да"
			}
			distributionRows = append(distributionRows, []xlsxCell{
				xlsxNumber(q.Index),
				xlsxNumber(q.QuestionID),
				xlsxText(q.Text),
				xlsxNumber(opt.OptionID),
				xlsxText(nonEmptyDash(opt.OptionText)),
				xlsxText(isCorrect),
				xlsxNumber(opt.Count),
				xlsxPercent(opt.Percent / 100.0),
			})
		}
		for _, accepted := range q.AcceptedAnswers {
			distributionRows = append(distributionRows, []xlsxCell{
				xlsxNumber(q.Index),
				xlsxNumber(q.QuestionID),
				xlsxText(q.Text),
				xlsxBlank(),
				xlsxText("Допустимый ответ: " + accepted),
				xlsxText("Да"),
				xlsxBlank(),
				xlsxBlank(),
			})
		}
		if q.SkippedCount > 0 {
			distributionRows = append(distributionRows, []xlsxCell{
				xlsxNumber(q.Index),
				xlsxNumber(q.QuestionID),
				xlsxText(q.Text),
				xlsxBlank(),
				xlsxText("Пропуск / пустой ответ"),
				xlsxText("—"),
				xlsxNumber(q.SkippedCount),
				xlsxPercent(q.SkippedPct / 100.0),
			})
		}
	}

	sheets := []xlsxSheet{
		{Name: "Сводка", Rows: summaryRows, Widths: []float64{26, 44}},
		{Name: "Попытки", Rows: attemptRows, Widths: []float64{8, 14, 24, 20, 10, 22, 22, 14, 10, 10, 12, 14}, FreezePane: true, AutoFilter: true},
		{Name: "Ответы", Rows: answerRows, Widths: []float64{14, 24, 20, 10, 12, 14, 50, 24, 12, 42, 42, 16}, FreezePane: true, AutoFilter: true},
		{Name: "Вопросы", Rows: questionRows, Widths: []float64{8, 14, 50, 24, 10, 10, 10, 12, 16, 12, 12}, FreezePane: true, AutoFilter: true},
		{Name: "Варианты", Rows: distributionRows, Widths: []float64{12, 14, 50, 12, 42, 12, 12, 12}, FreezePane: true, AutoFilter: true},
	}

	if len(data.TeamLeaderboard) > 0 {
		teamRows := [][]xlsxCell{{xlsxHeader("Место"), xlsxHeader("Команда"), xlsxHeader("Участников"), xlsxHeader("Баллы")}}
		for _, team := range data.TeamLeaderboard {
			teamRows = append(teamRows, []xlsxCell{
				xlsxNumber(team.Rank),
				xlsxText(team.TeamName),
				xlsxNumber(team.Members),
				xlsxNumber(team.Score),
			})
		}
		sheets = append(sheets, xlsxSheet{Name: "Команды", Rows: teamRows, Widths: []float64{10, 28, 14, 12}, FreezePane: true, AutoFilter: true})
	}

	return sheets
}

func correctAnswersByQuestionID(questions []quizStatsQuestion) map[int]string {
	out := make(map[int]string, len(questions))
	for _, q := range questions {
		parts := make([]string, 0, len(q.Options)+len(q.AcceptedAnswers))
		seen := make(map[string]struct{}, len(q.Options)+len(q.AcceptedAnswers))
		add := func(value string) {
			value = strings.TrimSpace(value)
			if value == "" {
				return
			}
			key := normalizeOpenAnswer(value)
			if key == "" {
				key = strings.ToLower(value)
			}
			if _, exists := seen[key]; exists {
				return
			}
			seen[key] = struct{}{}
			parts = append(parts, value)
		}
		for _, opt := range q.Options {
			if opt.IsCorrect {
				add(opt.OptionText)
			}
		}
		for _, accepted := range q.AcceptedAnswers {
			add(accepted)
		}
		out[q.QuestionID] = strings.Join(parts, "; ")
	}
	return out
}

func roundFloat(v float64, precision int) float64 {
	if precision < 0 {
		precision = 0
	}
	factor := 1.0
	for i := 0; i < precision; i++ {
		factor *= 10
	}
	if v >= 0 {
		return float64(int(v*factor+0.5)) / factor
	}
	return float64(int(v*factor-0.5)) / factor
}

func nonEmptyDash(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "—"
	}
	return value
}

func addXLSXPart(zw *zip.Writer, name string, body string) error {
	header := &zip.FileHeader{Name: name, Method: zip.Deflate}
	header.SetModTime(time.Now())
	part, err := zw.CreateHeader(header)
	if err != nil {
		return err
	}
	_, err = part.Write([]byte(body))
	return err
}

func xlsxContentTypesXML(sheetCount int) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	b.WriteString(`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">`)
	b.WriteString(`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>`)
	b.WriteString(`<Default Extension="xml" ContentType="application/xml"/>`)
	b.WriteString(`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>`)
	b.WriteString(`<Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>`)
	for i := 1; i <= sheetCount; i++ {
		b.WriteString(fmt.Sprintf(`<Override PartName="/xl/worksheets/sheet%d.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>`, i))
	}
	b.WriteString(`</Types>`)
	return b.String()
}

func xlsxRootRelsXML() string {
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/></Relationships>`
}

func xlsxWorkbookXML(sheets []xlsxSheet) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	b.WriteString(`<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets>`)
	for i, sheet := range sheets {
		b.WriteString(fmt.Sprintf(`<sheet name="%s" sheetId="%d" r:id="rId%d"/>`, xmlAttr(sheet.Name), i+1, i+1))
	}
	b.WriteString(`</sheets></workbook>`)
	return b.String()
}

func xlsxWorkbookRelsXML(sheetCount int) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	b.WriteString(`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`)
	for i := 1; i <= sheetCount; i++ {
		b.WriteString(fmt.Sprintf(`<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet%d.xml"/>`, i, i))
	}
	b.WriteString(fmt.Sprintf(`<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>`, sheetCount+1))
	b.WriteString(`</Relationships>`)
	return b.String()
}

func xlsxStylesXML() string {
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">
  <fonts count="2">
    <font><sz val="11"/><name val="Calibri"/></font>
    <font><b/><sz val="11"/><name val="Calibri"/></font>
  </fonts>
  <fills count="3">
    <fill><patternFill patternType="none"/></fill>
    <fill><patternFill patternType="gray125"/></fill>
    <fill><patternFill patternType="solid"><fgColor rgb="FFD9E1F2"/><bgColor indexed="64"/></patternFill></fill>
  </fills>
  <borders count="2">
    <border><left/><right/><top/><bottom/><diagonal/></border>
    <border><left style="thin"><color rgb="FFD9D9D9"/></left><right style="thin"><color rgb="FFD9D9D9"/></right><top style="thin"><color rgb="FFD9D9D9"/></top><bottom style="thin"><color rgb="FFD9D9D9"/></bottom><diagonal/></border>
  </borders>
  <cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs>
  <cellXfs count="5">
    <xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0"/>
    <xf numFmtId="0" fontId="1" fillId="2" borderId="1" xfId="0" applyFont="1" applyFill="1" applyBorder="1"/>
    <xf numFmtId="0" fontId="1" fillId="0" borderId="0" xfId="0" applyFont="1"/>
    <xf numFmtId="10" fontId="0" fillId="0" borderId="1" xfId="0" applyNumberFormat="1" applyBorder="1"/>
    <xf numFmtId="0" fontId="0" fillId="0" borderId="1" xfId="0" applyBorder="1"/>
  </cellXfs>
  <cellStyles count="1"><cellStyle name="Normal" xfId="0" builtinId="0"/></cellStyles>
</styleSheet>`
}

func xlsxWorksheetXML(sheet xlsxSheet) string {
	maxRow := len(sheet.Rows)
	maxCol := 1
	for _, row := range sheet.Rows {
		if len(row) > maxCol {
			maxCol = len(row)
		}
	}
	if maxRow < 1 {
		maxRow = 1
	}

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	b.WriteString(`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">`)
	b.WriteString(fmt.Sprintf(`<dimension ref="A1:%s%d"/>`, xlsxColumnName(maxCol), maxRow))
	if sheet.FreezePane {
		b.WriteString(`<sheetViews><sheetView workbookViewId="0"><pane ySplit="1" topLeftCell="A2" activePane="bottomLeft" state="frozen"/><selection pane="bottomLeft" activeCell="A2" sqref="A2"/></sheetView></sheetViews>`)
	} else {
		b.WriteString(`<sheetViews><sheetView workbookViewId="0"/></sheetViews>`)
	}
	if len(sheet.Widths) > 0 {
		b.WriteString(`<cols>`)
		for i, width := range sheet.Widths {
			if width <= 0 {
				continue
			}
			b.WriteString(fmt.Sprintf(`<col min="%d" max="%d" width="%.2f" customWidth="1"/>`, i+1, i+1, width))
		}
		b.WriteString(`</cols>`)
	}
	b.WriteString(`<sheetData>`)
	for rowIdx, row := range sheet.Rows {
		r := rowIdx + 1
		b.WriteString(fmt.Sprintf(`<row r="%d">`, r))
		for colIdx, cell := range row {
			b.WriteString(xlsxCellXML(colIdx+1, r, cell))
		}
		b.WriteString(`</row>`)
	}
	b.WriteString(`</sheetData>`)
	if sheet.AutoFilter && len(sheet.Rows) > 1 && maxCol > 1 {
		b.WriteString(fmt.Sprintf(`<autoFilter ref="A1:%s%d"/>`, xlsxColumnName(maxCol), len(sheet.Rows)))
	}
	b.WriteString(`</worksheet>`)
	return b.String()
}

func xlsxCellXML(col, row int, cell xlsxCell) string {
	ref := xlsxColumnName(col) + strconv.Itoa(row)
	styleAttr := ""
	if cell.Style > 0 {
		styleAttr = fmt.Sprintf(` s="%d"`, cell.Style)
	}
	if cell.Value == nil {
		return fmt.Sprintf(`<c r="%s"%s/>`, ref, styleAttr)
	}
	switch v := cell.Value.(type) {
	case string:
		return fmt.Sprintf(`<c r="%s"%s t="inlineStr"><is><t>%s</t></is></c>`, ref, styleAttr, xmlText(v))
	case int:
		return fmt.Sprintf(`<c r="%s"%s><v>%d</v></c>`, ref, styleAttr, v)
	case int64:
		return fmt.Sprintf(`<c r="%s"%s><v>%d</v></c>`, ref, styleAttr, v)
	case float64:
		return fmt.Sprintf(`<c r="%s"%s><v>%s</v></c>`, ref, styleAttr, strconv.FormatFloat(v, 'f', -1, 64))
	case bool:
		if v {
			return fmt.Sprintf(`<c r="%s"%s t="b"><v>1</v></c>`, ref, styleAttr)
		}
		return fmt.Sprintf(`<c r="%s"%s t="b"><v>0</v></c>`, ref, styleAttr)
	default:
		return fmt.Sprintf(`<c r="%s"%s t="inlineStr"><is><t>%s</t></is></c>`, ref, styleAttr, xmlText(fmt.Sprint(v)))
	}
}

func xlsxColumnName(col int) string {
	if col <= 0 {
		return "A"
	}
	var chars []byte
	for col > 0 {
		col--
		chars = append([]byte{byte('A' + col%26)}, chars...)
		col /= 26
	}
	return string(chars)
}

func cleanXMLString(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			return r
		case r >= 0x20:
			return r
		default:
			return -1
		}
	}, s)
}

func xmlText(s string) string {
	s = cleanXMLString(s)
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func xmlAttr(s string) string {
	return strings.ReplaceAll(xmlText(s), `"`, "&quot;")
}

func renderQuizParticipantStats(w http.ResponseWriter, r *http.Request, currentUsername string, quizID int, sessionID int64) {
	var q quiz
	err := db.QueryRow("SELECT "+quizSelectColumns+" FROM quizzes WHERE id = ?", quizID).Scan(&q.Id, &q.Title, &q.Description, &q.Category, &q.Author, &q.PrivateKey, &q.Privacy, &q.Status, &q.TimeLimitSeconds, &q.PassScore, &q.ParticipationMode, &q.TeamCount)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		log.Printf("Ошибка чтения квиза %d для статистики участника: %v", quizID, err)
		http.Error(w, "Ошибка базы данных", http.StatusInternalServerError)
		return
	}
	if q.Author != currentUsername {
		http.Error(w, "Доступ запрещён", http.StatusForbidden)
		return
	}
	var userIDValue sql.NullInt64
	var participantName string
	var teamName string
	var attempt int
	var startedAt, finishedAt time.Time
	var score, maxScore int
	err = db.QueryRow(
		`SELECT 
			qs.user_id,
			COALESCE(NULLIF(TRIM(qs.participant_name), ''), 'Участник') AS participant_name,
			COALESCE(NULLIF(TRIM(qs.team_name), ''), '') AS team_name,
			qs.attempt_number,
			qs.started_at,
			qs.finished_at,
			qs.score,
			qs.max_score
		FROM quiz_sessions qs
		LEFT JOIN users u ON u.id = qs.user_id
		WHERE qs.id = ? AND qs.quiz_id = ?`, sessionID, quizID,
	).Scan(&userIDValue, &participantName, &teamName, &attempt, &startedAt, &finishedAt, &score, &maxScore)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		log.Printf("Ошибка чтения сессии %d квиза %d: %v", sessionID, quizID, err)
		http.Error(w, "Ошибка базы данных", http.StatusInternalServerError)
		return
	}

	qRows, err := db.Query("SELECT id, text, type, points, author_id, quiz_id, COALESCE(question_media_url, ''), COALESCE(question_media_type, '') FROM questions WHERE quiz_id = ? ORDER BY id", quizID)
	if err != nil {
		log.Printf("Ошибка загрузки вопросов квиза %d для статистики участника: %v", quizID, err)
		http.Error(w, "Ошибка базы данных", http.StatusInternalServerError)
		return
	}
	defer qRows.Close()

	type optView struct {
		ID         int
		Text       string
		ImageURL   string
		IsCorrect  bool
		IsSelected bool
	}
	type qView struct {
		Index             int
		QuestionID        int
		Text              string
		Type              string
		TypeLabel         string
		Points            int
		Earned            int
		Status            string
		StatusLabel       string
		UserText          string
		QuestionMediaURL  string
		QuestionMediaType string
		AcceptedAnswers   []string
		Options           []optView
	}

	typeLabel := func(t string) string {
		switch t {
		case "single":
			return "Одиночный выбор"
		case "multiple":
			return "Множественный выбор"
		case "photo_single":
			return "Фото-одиночный выбор"
		case "photo_multiple":
			return "Фото-множественный выбор"
		case "open":
			return "Открытый вопрос"
		case "photo":
			return "Фото"
		case "video":
			return "Видео"
		default:
			return t
		}
	}

	views := make([]qView, 0, 32)
	idx := 0
	for qRows.Next() {
		var qu question
		var questionMediaURL, questionMediaType string
		if err := qRows.Scan(&qu.Id, &qu.Text, &qu.Type, &qu.Points, &qu.AuthorID, &qu.QuizID, &questionMediaURL, &questionMediaType); err != nil {
			log.Printf("Ошибка чтения вопроса квиза %d для статистики участника: %v", quizID, err)
			http.Error(w, "Ошибка базы данных", http.StatusInternalServerError)
			return
		}
		qu.QuestionMediaURL, qu.QuestionMediaType = normalizeQuestionMedia(questionMediaURL, questionMediaType)
		idx++
		v := qView{
			Index:             idx,
			QuestionID:        qu.Id,
			Text:              qu.Text,
			Type:              qu.Type,
			TypeLabel:         typeLabel(qu.Type),
			Points:            qu.Points,
			QuestionMediaURL:  qu.QuestionMediaURL,
			QuestionMediaType: qu.QuestionMediaType,
		}

		ansRows, err := db.Query(
			`SELECT selected_option_id, answer_text, is_correct
			FROM user_answers
			WHERE session_id = ? AND question_id = ?`, sessionID, qu.Id,
		)
		if err != nil {
			log.Printf("Ошибка загрузки ответов сессии %d по вопросу %d: %v", sessionID, qu.Id, err)
			http.Error(w, "Ошибка базы данных", http.StatusInternalServerError)
			return
		}
		selected := make([]int, 0, 4)
		var userText string
		var isCorr sql.NullBool
		for ansRows.Next() {
			var optID sql.NullInt64
			var txt sql.NullString
			var ic sql.NullBool
			if err := ansRows.Scan(&optID, &txt, &ic); err != nil {
				_ = ansRows.Close()
				log.Printf("Ошибка чтения ответа сессии %d по вопросу %d: %v", sessionID, qu.Id, err)
				http.Error(w, "Ошибка базы данных", http.StatusInternalServerError)
				return
			}
			if optID.Valid {
				selected = append(selected, int(optID.Int64))
			}
			if txt.Valid && strings.TrimSpace(txt.String) != "" {
				userText = txt.String
			}
			if ic.Valid {
				isCorr = ic
			}
		}
		_ = ansRows.Close()
		selected = uniqueSortedInts(selected)
		v.UserText = userText

		optRows, err := db.Query("SELECT id, option_text, option_image_url, is_correct FROM question_options WHERE question_id = ? ORDER BY id", qu.Id)
		if err != nil {
			log.Printf("Ошибка загрузки вариантов вопроса %d: %v", qu.Id, err)
			http.Error(w, "Ошибка базы данных", http.StatusInternalServerError)
			return
		}
		selSet := makeIntSet(selected)
		opts := make([]optView, 0, 8)
		acceptedAnswers := make([]string, 0, 4)
		acceptedSeen := make(map[string]struct{}, 4)
		correctOptionsCount := 0
		for optRows.Next() {
			var oid int
			var txt string
			var img sql.NullString
			var ic bool
			if err := optRows.Scan(&oid, &txt, &img, &ic); err != nil {
				_ = optRows.Close()
				http.Error(w, "Ошибка базы данных", http.StatusInternalServerError)
				return
			}
			cleanText := strings.TrimSpace(txt)
			_, sel := selSet[oid]
			opts = append(opts, optView{ID: oid, Text: cleanText, ImageURL: nullStringToString(img), IsCorrect: ic, IsSelected: sel})
			if ic {
				correctOptionsCount++
			}
			if isOpenQuestionType(qu.Type) && ic && cleanText != "" {
				norm := normalizeOpenAnswer(cleanText)
				if norm != "" {
					if _, exists := acceptedSeen[norm]; !exists {
						acceptedSeen[norm] = struct{}{}
						acceptedAnswers = append(acceptedAnswers, cleanText)
					}
				}
			}
		}
		_ = optRows.Close()
		v.Options = opts
		v.AcceptedAnswers = acceptedAnswers

		switch {
		case isCorr.Valid && isCorr.Bool:
			v.Status = "correct"
			v.StatusLabel = "Верно"
			v.Earned = qu.Points
		case isCorr.Valid && !isCorr.Bool:
			v.Status = "wrong"
			v.StatusLabel = "Неверно"
			v.Earned = 0
		default:
			v.Status = "unchecked"
			v.Earned = 0
			switch {
			case isOpenQuestionType(qu.Type) && len(acceptedAnswers) == 0:
				v.StatusLabel = "Не проверяется автоматически"
			case !isOpenQuestionType(qu.Type) && isChoiceQuestionType(qu.Type) && correctOptionsCount == 0:
				v.StatusLabel = "Нет заданного правильного ответа"
			case !isChoiceQuestionType(qu.Type) && !isOpenQuestionType(qu.Type):
				v.StatusLabel = "Не проверяется автоматически"
			default:
				v.StatusLabel = "Без автоматической оценки"
			}
		}

		views = append(views, v)
	}

	durationHuman := formatDurationHuman(finishedAt.Sub(startedAt))
	if finishedAt.Before(startedAt) {
		durationHuman = formatDurationHuman(0)
	}

	data := struct {
		Username    string
		Quiz        quiz
		SessionID   int64
		Participant string
		TeamName    string
		Attempt     int
		StartedAt   string
		FinishedAt  string
		Duration    string
		Score       int
		MaxScore    int
		Passed      bool
		Questions   []qView
	}{
		Username:    currentUsername,
		Quiz:        q,
		SessionID:   sessionID,
		Participant: participantName,
		TeamName:    teamName,
		Attempt:     attempt,
		StartedAt:   startedAt.Format("2006-01-02 15:04:05"),
		FinishedAt:  finishedAt.Format("2006-01-02 15:04:05"),
		Duration:    durationHuman,
		Score:       score,
		MaxScore:    maxScore,
		Passed:      q.PassScore <= 0 || score >= q.PassScore,
		Questions:   views,
	}

	tmpl, err := template.ParseFiles("frontend/quizParticipant.html")
	if err != nil {
		http.Error(w, "Не найден шаблон quizParticipant.html", http.StatusInternalServerError)
		return
	}
	_ = tmpl.Execute(w, data)
}

func handleHistory(w http.ResponseWriter, r *http.Request) {
	currentUsername, _ := getCurrentUsername(r)
	if currentUsername == "" {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	requested := strings.TrimPrefix(r.URL.Path, "/history/")
	if requested != currentUsername {
		http.Error(w, "Доступ запрещён", http.StatusForbidden)
		return
	}

	userID, ok := getUserID(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	type historyRow struct {
		QuizID           int
		Title            string
		Description      string
		Category         string
		Author           string
		Privacy          string
		Attempts         int
		AvgScore         float64
		AvgMaxScore      float64
		AvgPercent       float64
		AvgDurationSec   float64
		AvgDurationHuman string
		LastFinished     string
		LastFinishedT    time.Time
	}

	rows, err := db.Query(
		`SELECT 
			q.id, q.title, q.description, q.category, q.author, q.privacy,
			COUNT(*) AS attempts,
			COALESCE(AVG(s.score), 0) AS avg_score,
			COALESCE(AVG(s.max_score), 0) AS avg_max,
			COALESCE(AVG(TIMESTAMPDIFF(SECOND, s.started_at, s.finished_at)), 0) AS avg_duration_sec,
			MAX(s.finished_at) AS last_finished
		FROM quiz_sessions s
		JOIN quizzes q ON q.id = s.quiz_id
		WHERE s.user_id = ?
		GROUP BY q.id, q.title, q.description, q.category, q.author, q.privacy
		ORDER BY last_finished DESC, q.id DESC`,
		userID,
	)
	if err != nil {
		http.Error(w, "Ошибка базы данных", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	history := make([]historyRow, 0, 32)
	for rows.Next() {
		var hr historyRow
		var last time.Time
		if err := rows.Scan(&hr.QuizID, &hr.Title, &hr.Description, &hr.Category, &hr.Author, &hr.Privacy, &hr.Attempts, &hr.AvgScore, &hr.AvgMaxScore, &hr.AvgDurationSec, &last); err != nil {
			http.Error(w, "Ошибка чтения данных", http.StatusInternalServerError)
			return
		}
		hr.LastFinishedT = last
		hr.LastFinished = last.Format("2006-01-02 15:04:05")
		hr.AvgDurationHuman = formatDurationHuman(time.Duration(int(hr.AvgDurationSec+0.5)) * time.Second)
		if hr.AvgMaxScore > 0 {
			hr.AvgPercent = (hr.AvgScore / hr.AvgMaxScore) * 100.0
		}
		history = append(history, hr)
	}

	data := struct {
		Username string
		Rows     []historyRow
	}{
		Username: currentUsername,
		Rows:     history,
	}

	tmpl, err := template.ParseFiles("frontend/history.html")
	if err != nil {
		http.Error(w, "Не найден шаблон history.html", http.StatusInternalServerError)
		return
	}
	if err := tmpl.Execute(w, data); err != nil {
		http.Error(w, "Ошибка рендера страницы", http.StatusInternalServerError)
		return
	}
}

func handleChangeUsername(w http.ResponseWriter, r *http.Request) {
	wantJSON := wantsJSON(r)
	if !isAuthenticated(r) {
		if wantJSON {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "Требуется авторизация"})
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	userID, ok := getUserID(r)
	if !ok {
		if wantJSON {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "Требуется авторизация"})
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	currentUsername, _ := getCurrentUsername(r)

	type pageData struct {
		Username string
		Error    string
		Success  string
	}

	data := pageData{Username: currentUsername}
	if r.URL.Query().Get("success") == "1" {
		data.Success = "Имя пользователя успешно изменено"
	}

	if r.Method == http.MethodGet {
		tmpl, err := template.ParseFiles("frontend/changeUsername.html")
		if err != nil {
			log.Printf("Ошибка загрузки шаблона changeUsername.html: %v", err)
			http.Error(w, "Ошибка шаблона", http.StatusInternalServerError)
			return
		}
		_ = tmpl.Execute(w, data)
		return
	}

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	newUsername := strings.TrimSpace(r.FormValue("new_username"))
	password := r.FormValue("password")

	if !usernamePattern.MatchString(newUsername) {
		msg := "Имя пользователя должно быть минимум 3 символа и содержать только латинские буквы, цифры и _"
		if wantJSON {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": msg})
			return
		}
		data.Error = msg
		tmpl, _ := template.ParseFiles("frontend/changeUsername.html")
		_ = tmpl.Execute(w, data)
		return
	}

	if newUsername == currentUsername {
		msg := "Новое имя пользователя совпадает с текущим"
		if wantJSON {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": msg})
			return
		}
		data.Error = msg
		tmpl, _ := template.ParseFiles("frontend/changeUsername.html")
		_ = tmpl.Execute(w, data)
		return
	}

	var dbPassword string
	err := db.QueryRow("SELECT password FROM users WHERE id = ?", userID).Scan(&dbPassword)
	if err != nil {
		msg := "Ошибка базы данных"
		if wantJSON {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": msg})
			return
		}
		data.Error = msg
		tmpl, _ := template.ParseFiles("frontend/changeUsername.html")
		_ = tmpl.Execute(w, data)
		return
	}
	if password != dbPassword {
		msg := "Неверный пароль"
		if wantJSON {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": msg})
			return
		}
		data.Error = msg
		tmpl, _ := template.ParseFiles("frontend/changeUsername.html")
		_ = tmpl.Execute(w, data)
		return
	}

	var exists int
	db.QueryRow("SELECT COUNT(*) FROM users WHERE username = ?", newUsername).Scan(&exists)
	if exists > 0 {
		msg := "Имя пользователя занято"
		if wantJSON {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": msg})
			return
		}
		data.Error = msg
		tmpl, _ := template.ParseFiles("frontend/changeUsername.html")
		_ = tmpl.Execute(w, data)
		return
	}

	db.Exec("UPDATE users SET username = ? WHERE id = ?", newUsername, userID)
	db.Exec("UPDATE quizzes SET author = ? WHERE author = ?", newUsername, currentUsername)

	session, _ := store.Get(r, "session-name")
	session.Values["username"] = newUsername
	_ = session.Save(r, w)

	if wantJSON {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "Имя пользователя успешно изменено", "username": newUsername})
		return
	}

	http.Redirect(w, r, "/profile/change-username?success=1", http.StatusSeeOther)
}

func handleChangePassword(w http.ResponseWriter, r *http.Request) {
	wantJSON := wantsJSON(r)
	if !isAuthenticated(r) {
		if wantJSON {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "Требуется авторизация"})
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	userID, ok := getUserID(r)
	if !ok {
		if wantJSON {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "Требуется авторизация"})
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	currentUsername, _ := getCurrentUsername(r)

	type pageData struct {
		Username string
		Error    string
		Success  string
	}

	data := pageData{Username: currentUsername}
	if r.URL.Query().Get("success") == "1" {
		data.Success = "Пароль успешно изменён"
	}

	if r.Method == http.MethodGet {
		tmpl, err := template.ParseFiles("frontend/changePassword.html")
		if err != nil {
			log.Printf("Ошибка загрузки шаблона changePassword.html: %v", err)
			http.Error(w, "Ошибка шаблона", http.StatusInternalServerError)
			return
		}
		_ = tmpl.Execute(w, data)
		return
	}

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	currentPassword := r.FormValue("current_password")
	newPassword := r.FormValue("new_password")
	confirmPassword := r.FormValue("confirm_password")

	if len(newPassword) < 6 {
		msg := "Новый пароль должен быть не менее 6 символов"
		if wantJSON {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": msg})
			return
		}
		data.Error = msg
		tmpl, _ := template.ParseFiles("frontend/changePassword.html")
		_ = tmpl.Execute(w, data)
		return
	}

	if newPassword != confirmPassword {
		msg := "Новый пароль и подтверждение не совпадают"
		if wantJSON {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": msg})
			return
		}
		data.Error = msg
		tmpl, _ := template.ParseFiles("frontend/changePassword.html")
		_ = tmpl.Execute(w, data)
		return
	}

	var dbPassword string
	err := db.QueryRow("SELECT password FROM users WHERE id = ?", userID).Scan(&dbPassword)
	if err != nil {
		msg := "Ошибка базы данных"
		if wantJSON {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": msg})
			return
		}
		data.Error = msg
		tmpl, _ := template.ParseFiles("frontend/changePassword.html")
		_ = tmpl.Execute(w, data)
		return
	}
	if currentPassword != dbPassword {
		msg := "Текущий пароль неверный"
		if wantJSON {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": msg})
			return
		}
		data.Error = msg
		tmpl, _ := template.ParseFiles("frontend/changePassword.html")
		_ = tmpl.Execute(w, data)
		return
	}

	_, err = db.Exec("UPDATE users SET password = ? WHERE id = ?", newPassword, userID)
	if err != nil {
		msg := "Не удалось обновить пароль"
		if wantJSON {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": msg})
			return
		}
		data.Error = msg
		tmpl, _ := template.ParseFiles("frontend/changePassword.html")
		_ = tmpl.Execute(w, data)
		return
	}

	if wantJSON {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "Пароль успешно изменён"})
		return
	}

	http.Redirect(w, r, "/profile/change-password?success=1", http.StatusSeeOther)
}

func handleQuestionsBank(w http.ResponseWriter, r *http.Request) {
	if !isAuthenticated(r) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	userID, _ := getUserID(r)
	username, _ := getCurrentUsername(r)

	if wantsJSON(r) {
		action := strings.TrimSpace(r.URL.Query().Get("action"))
		if action == "" {
			action = "list"
		}

		switch r.Method {
		case http.MethodGet:
			switch action {
			case "list":
				items, err := loadBankQuestionsForUser(userID)
				if err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "Ошибка чтения банка вопросов"})
					return
				}
				writeJSON(w, http.StatusOK, map[string]any{"ok": true, "items": items})
				return
			case "get":
				id, _ := strconv.Atoi(r.URL.Query().Get("id"))
				item, err := loadOneBankQuestion(userID, id)
				if err != nil {
					writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "Вопрос не найден"})
					return
				}
				writeJSON(w, http.StatusOK, map[string]any{"ok": true, "item": item})
				return
			default:
				writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "Неизвестное действие"})
				return
			}
		case http.MethodPost:
			var payload struct {
				Action            string            `json:"action"`
				Id                int               `json:"id"`
				Text              string            `json:"text"`
				QuestionMediaURL  string            `json:"question_media_url"`
				QuestionMediaType string            `json:"question_media_type"`
				QuestionImageURL  string            `json:"question_image_url"`
				Category          string            `json:"category"`
				Type              string            `json:"type"`
				Points            int               `json:"points"`
				Options           []BankOptionInput `json:"options"`
			}

			ct := r.Header.Get("Content-Type")
			if strings.Contains(ct, "application/json") {
				_ = json.NewDecoder(r.Body).Decode(&payload)
			} else {
				_ = r.ParseForm()
				payload.Action = strings.TrimSpace(r.FormValue("action"))
				payload.Text = strings.TrimSpace(r.FormValue("text"))
				payload.QuestionMediaURL = strings.TrimSpace(r.FormValue("question_media_url"))
				payload.QuestionMediaType = strings.TrimSpace(r.FormValue("question_media_type"))
				payload.QuestionImageURL = strings.TrimSpace(r.FormValue("question_image_url"))
				payload.Category = strings.TrimSpace(r.FormValue("category"))
				payload.Type = strings.TrimSpace(r.FormValue("type"))
				payload.Id, _ = strconv.Atoi(r.FormValue("id"))
				payload.Points, _ = strconv.Atoi(r.FormValue("points"))

				cnt, _ := strconv.Atoi(r.FormValue("option_count"))
				for i := 1; i <= cnt; i++ {
					txt := strings.TrimSpace(r.FormValue("option_text_" + strconv.Itoa(i)))
					if txt == "" {
						continue
					}
					ic := r.FormValue("option_correct_"+strconv.Itoa(i)) == "on" || r.FormValue("option_correct_"+strconv.Itoa(i)) == "1"
					payload.Options = append(payload.Options, BankOptionInput{Text: txt, IsCorrect: ic})

				}
			}

			if strings.TrimSpace(payload.QuestionMediaURL) == "" {
				payload.QuestionMediaURL = strings.TrimSpace(payload.QuestionImageURL)
			}
			payload.QuestionMediaURL, payload.QuestionMediaType = normalizeQuestionMedia(payload.QuestionMediaURL, payload.QuestionMediaType)

			if payload.Action == "" {
				payload.Action = action
			}

			switch payload.Action {
			case "create":
				id, err := createBankQuestion(userID, payload.Text, payload.QuestionMediaURL, payload.QuestionMediaType, payload.Category, payload.Type, payload.Points, payload.Options)
				if err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "Не удалось создать вопрос"})
					return
				}
				writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
				return
			case "update":
				err := updateBankQuestion(userID, payload.Id, payload.Text, payload.QuestionMediaURL, payload.QuestionMediaType, payload.Category, payload.Type, payload.Points, payload.Options)
				if err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "Не удалось обновить вопрос"})
					return
				}
				writeJSON(w, http.StatusOK, map[string]any{"ok": true})
				return
			case "delete":
				err := deleteBankQuestion(userID, payload.Id)
				if err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "Не удалось удалить вопрос"})
					return
				}
				writeJSON(w, http.StatusOK, map[string]any{"ok": true})
				return
			default:
				writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "Неизвестное действие"})
				return
			}
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
	}
	data := struct {
		Username string
	}{
		Username: username,
	}
	tmpl, _ := template.ParseFiles("frontend/questionBank.html")
	err := tmpl.Execute(w, data)
	if err != nil {
		panic(err.Error())
	}
}

func loadBankQuestionsForUser(userID int) ([]BankQuestionWithOptions, error) {
	rows, err := db.Query(`
		SELECT id, text, COALESCE(question_image_url, '') as question_media_url, COALESCE(question_media_type, '') as question_media_type, category, type, points, author_id,
		       DATE_FORMAT(created_at, '%Y-%m-%d %H:%i:%s') as created_at,
		       DATE_FORMAT(updated_at, '%Y-%m-%d %H:%i:%s') as updated_at
		FROM question_bank
		WHERE author_id = ?
		ORDER BY id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]BankQuestionWithOptions, 0, 64)
	for rows.Next() {
		var q bankQuestion
		if err := rows.Scan(&q.Id, &q.Text, &q.QuestionMediaURL, &q.QuestionMediaType, &q.Category, &q.Type, &q.Points, &q.AuthorID, &q.CreatedAt, &q.UpdatedAt); err != nil {
			return nil, err
		}
		q.QuestionMediaURL, q.QuestionMediaType = normalizeQuestionMedia(q.QuestionMediaURL, q.QuestionMediaType)
		q.QuestionImageURL = q.QuestionMediaURL
		wq := BankQuestionWithOptions{Question: q}
		if isChoiceQuestionType(q.Type) || isOpenQuestionType(q.Type) {
			optRows, err := db.Query(`SELECT id, option_text, option_image_url, is_correct FROM question_bank_options WHERE bank_question_id = ? ORDER BY id`, q.Id)
			if err != nil {
				return nil, err
			}
			accepted := make([]string, 0, 4)
			acceptedSeen := make(map[string]struct{}, 4)
			for optRows.Next() {
				var (
					o struct {
						Id         int
						OptionText string
						ImageURL   string
						IsCorrect  bool
					}
					img sql.NullString
				)
				_ = optRows.Scan(&o.Id, &o.OptionText, &img, &o.IsCorrect)
				o.ImageURL = nullStringToString(img)
				o.OptionText = strings.TrimSpace(o.OptionText)
				if isChoiceQuestionType(q.Type) {
					wq.Options = append(wq.Options, o)
				}
				if isOpenQuestionType(q.Type) && o.IsCorrect && o.OptionText != "" {
					norm := normalizeOpenAnswer(o.OptionText)
					if norm != "" {
						if _, exists := acceptedSeen[norm]; !exists {
							acceptedSeen[norm] = struct{}{}
							accepted = append(accepted, o.OptionText)
						}
					}
				}
			}
			_ = optRows.Close()
			if len(accepted) > 0 {
				wq.OpenAnswersText = strings.Join(accepted, "\n")
			}
		}
		items = append(items, wq)
	}
	return items, nil
}

func loadOneBankQuestion(userID, id int) (*BankQuestionWithOptions, error) {
	var q bankQuestion
	err := db.QueryRow(`
		SELECT id, text, COALESCE(question_image_url, '') as question_media_url, COALESCE(question_media_type, '') as question_media_type, category, type, points, author_id,
		       DATE_FORMAT(created_at, '%Y-%m-%d %H:%i:%s') as created_at,
		       DATE_FORMAT(updated_at, '%Y-%m-%d %H:%i:%s') as updated_at
		FROM question_bank
		WHERE id = ? AND author_id = ?`, id, userID).Scan(&q.Id, &q.Text, &q.QuestionMediaURL, &q.QuestionMediaType, &q.Category, &q.Type, &q.Points, &q.AuthorID, &q.CreatedAt, &q.UpdatedAt)
	if err != nil {
		return nil, err
	}
	q.QuestionMediaURL, q.QuestionMediaType = normalizeQuestionMedia(q.QuestionMediaURL, q.QuestionMediaType)
	q.QuestionImageURL = q.QuestionMediaURL
	res := &BankQuestionWithOptions{Question: q}
	if isChoiceQuestionType(q.Type) || isOpenQuestionType(q.Type) {
		optRows, err := db.Query(`SELECT id, option_text, option_image_url, is_correct FROM question_bank_options WHERE bank_question_id = ? ORDER BY id`, q.Id)
		if err != nil {
			return nil, err
		}
		defer optRows.Close()
		accepted := make([]string, 0, 4)
		acceptedSeen := make(map[string]struct{}, 4)
		for optRows.Next() {
			var (
				o struct {
					Id         int
					OptionText string
					ImageURL   string
					IsCorrect  bool
				}
				img sql.NullString
			)
			_ = optRows.Scan(&o.Id, &o.OptionText, &img, &o.IsCorrect)
			o.ImageURL = nullStringToString(img)
			o.OptionText = strings.TrimSpace(o.OptionText)
			if isChoiceQuestionType(q.Type) {
				res.Options = append(res.Options, o)
			}
			if isOpenQuestionType(q.Type) && o.IsCorrect && o.OptionText != "" {
				norm := normalizeOpenAnswer(o.OptionText)
				if norm != "" {
					if _, exists := acceptedSeen[norm]; !exists {
						acceptedSeen[norm] = struct{}{}
						accepted = append(accepted, o.OptionText)
					}
				}
			}
		}
		if len(accepted) > 0 {
			res.OpenAnswersText = strings.Join(accepted, "\n")
		}
	}
	return res, nil
}

func createBankQuestion(userID int, text, questionMediaURL, questionMediaType, category, qType string, points int, options []BankOptionInput) (int64, error) {
	text = strings.TrimSpace(text)
	category = strings.TrimSpace(category)
	qType = strings.TrimSpace(qType)
	questionMediaURL, questionMediaType = normalizeQuestionMedia(questionMediaURL, questionMediaType)
	if text == "" {
		return 0, errors.New("empty text")
	}
	if category == "" {
		category = "other"
	}
	if points < 0 {
		points = 0
	}
	if qType == "" {
		qType = "single"
	}
	if qType != "single" && qType != "multiple" && qType != "photo_single" && qType != "photo_multiple" && qType != "open" && qType != "video" && qType != "photo" {
		return 0, errors.New("bad type")
	}

	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Exec(`INSERT INTO question_bank (text, question_image_url, question_media_type, category, type, points, author_id) VALUES (?, ?, ?, ?, ?, ?, ?)`, text, questionMediaURL, questionMediaType, category, qType, points, userID)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()

	if isChoiceQuestionType(qType) || isOpenQuestionType(qType) {
		clean := make([]BankOptionInput, 0, len(options))
		seenOpenAnswers := make(map[string]struct{}, len(options))
		for _, o := range options {
			ot := strings.TrimSpace(o.Text)
			img := strings.TrimSpace(o.ImageURL)
			if ot == "" && img == "" {
				continue
			}
			if isOpenQuestionType(qType) {
				norm := normalizeOpenAnswer(ot)
				if norm == "" {
					continue
				}
				if _, exists := seenOpenAnswers[norm]; exists {
					continue
				}
				seenOpenAnswers[norm] = struct{}{}
				clean = append(clean, BankOptionInput{Text: ot, ImageURL: "", IsCorrect: true})
				continue
			}
			clean = append(clean, BankOptionInput{Text: ot, ImageURL: img, IsCorrect: o.IsCorrect})
		}
		if isChoiceQuestionType(qType) {
			if len(clean) < 2 {
				return 0, errors.New("need options")
			}
			if isSingleChoiceQuestionType(qType) {
				seen := false
				for i := range clean {
					if clean[i].IsCorrect {
						if !seen {
							seen = true
						} else {
							clean[i].IsCorrect = false
						}
					}
				}
				if !seen {
					clean[0].IsCorrect = true
				}
			}
		}
		for _, o := range clean {
			img := strings.TrimSpace(o.ImageURL)
			_, err = tx.Exec(`INSERT INTO question_bank_options (bank_question_id, option_text, option_image_url, is_correct) VALUES (?, ?, ?, ?)`, id, o.Text, img, o.IsCorrect)
			if err != nil {
				return 0, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

func updateBankQuestion(userID, id int, text, questionMediaURL, questionMediaType, category, qType string, points int, options []BankOptionInput) error {
	text = strings.TrimSpace(text)
	category = strings.TrimSpace(category)
	qType = strings.TrimSpace(qType)
	questionMediaURL, questionMediaType = normalizeQuestionMedia(questionMediaURL, questionMediaType)
	if id <= 0 {
		return errors.New("bad id")
	}
	if text == "" {
		return errors.New("empty text")
	}
	if category == "" {
		category = "other"
	}
	if points < 0 {
		points = 0
	}
	if qType == "" {
		qType = "single"
	}
	if qType != "single" && qType != "multiple" && qType != "photo_single" && qType != "photo_multiple" && qType != "open" && qType != "video" && qType != "photo" {
		return errors.New("bad type")
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Exec(`UPDATE question_bank SET text = ?, question_image_url = ?, question_media_type = ?, category = ?, type = ?, points = ? WHERE id = ? AND author_id = ?`, text, questionMediaURL, questionMediaType, category, qType, points, id, userID)
	if err != nil {
		return err
	}
	aff, _ := res.RowsAffected()
	if aff == 0 {
		return sql.ErrNoRows
	}

	_, err = tx.Exec(`DELETE FROM question_bank_options WHERE bank_question_id = ?`, id)
	if err != nil {
		return err
	}

	if isChoiceQuestionType(qType) || isOpenQuestionType(qType) {
		clean := make([]BankOptionInput, 0, len(options))
		seenOpenAnswers := make(map[string]struct{}, len(options))
		for _, o := range options {
			ot := strings.TrimSpace(o.Text)
			img := strings.TrimSpace(o.ImageURL)
			if ot == "" && img == "" {
				continue
			}
			if isOpenQuestionType(qType) {
				norm := normalizeOpenAnswer(ot)
				if norm == "" {
					continue
				}
				if _, exists := seenOpenAnswers[norm]; exists {
					continue
				}
				seenOpenAnswers[norm] = struct{}{}
				clean = append(clean, BankOptionInput{Text: ot, ImageURL: "", IsCorrect: true})
				continue
			}
			clean = append(clean, BankOptionInput{Text: ot, ImageURL: img, IsCorrect: o.IsCorrect})
		}
		if isChoiceQuestionType(qType) {
			if len(clean) < 2 {
				return errors.New("need options")
			}
			if isSingleChoiceQuestionType(qType) {
				seen := false
				for i := range clean {
					if clean[i].IsCorrect {
						if !seen {
							seen = true
						} else {
							clean[i].IsCorrect = false
						}
					}
				}
				if !seen {
					clean[0].IsCorrect = true
				}
			}
		}
		for _, o := range clean {
			img := strings.TrimSpace(o.ImageURL)
			_, err = tx.Exec(`INSERT INTO question_bank_options (bank_question_id, option_text, option_image_url, is_correct) VALUES (?, ?, ?, ?)`, id, o.Text, img, o.IsCorrect)
			if err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func deleteBankQuestion(userID, id int) error {
	if id <= 0 {
		return errors.New("bad id")
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.Exec(`DELETE FROM question_bank_options WHERE bank_question_id = ?`, id)
	if err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM question_bank WHERE id = ? AND author_id = ?`, id, userID)
	if err != nil {
		return err
	}
	aff, _ := res.RowsAffected()
	if aff == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}
