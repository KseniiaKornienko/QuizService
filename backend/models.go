package main

type quiz struct {
	Id                int
	Title             string
	Description       string
	Category          string
	Author            string
	Privacy           string
	PrivateKey        string
	Status            string
	TimeLimitSeconds  int
	PassScore         int
	ParticipationMode string
	TeamCount         int
}

type question struct {
	Id                int
	Text              string
	Type              string
	Points            int
	AuthorID          int
	QuizID            int
	QuestionMediaURL  string
	QuestionMediaType string
}

type QuestionWithOptions struct {
	Question question
	Options  []struct {
		Id         int
		OptionText string
		ImageURL   string
		IsCorrect  bool
	}
	OpenAnswersText string
}

type QuizResult struct {
	SessionID       int64
	AttemptNumber   int
	Score           int
	MaxScore        int
	DurationSeconds int
	DurationHuman   string
	Passed          bool
	HasPassScore    bool
	PassScore       int
	TeamName        string
	TeamRank        int
	TeamsTotal      int
}

type TeamStanding struct {
	Rank      int
	TeamName  string
	Score     int
	Members   int
	IsCurrent bool
}

type bankQuestion struct {
	Id                int
	Text              string
	QuestionMediaURL  string
	QuestionMediaType string
	QuestionImageURL  string
	Category          string
	Type              string
	Points            int
	AuthorID          int
	CreatedAt         string
	UpdatedAt         string
}

type BankQuestionWithOptions struct {
	Question bankQuestion
	Options  []struct {
		Id         int
		OptionText string
		ImageURL   string
		IsCorrect  bool
	}
	OpenAnswersText string
}
type BankOptionInput struct {
	Text      string `json:"text"`
	ImageURL  string `json:"image_url"`
	IsCorrect bool   `json:"is_correct"`
}

type editQuizPageData struct {
	Username      string
	Quiz          quiz
	Questions     []QuestionWithOptions
	ExistingCount int
	IsNew         bool
}

type quizFormOption struct {
	text      string
	imageURL  string
	isCorrect bool
}

type quizStatsSummary struct {
	QuizID            int
	Title             string
	LastStartedAt     string
	LastFinishedAt    string
	AttemptsTotal     int
	ParticipantsTotal int
	AvgScore          float64
	AvgMaxScore       float64
	TeamsTotal        int
}

type quizStatsOptionDist struct {
	OptionID       int
	OptionText     string
	OptionImageURL string
	Count          int
	Percent        float64
	IsCorrect      bool
}

type quizStatsQuestion struct {
	Index             int
	QuestionID        int
	Text              string
	Type              string
	Points            int
	QuestionMediaURL  string
	QuestionMediaType string
	Total             int
	Correct           int
	CorrectPct        float64
	HasAutoCheck      bool
	Options           []quizStatsOptionDist
	AcceptedAnswers   []string
	SkippedCount      int
	SkippedPct        float64
}

type quizStatsParticipantRow struct {
	SessionID       int64
	UserID          int
	ParticipantName string
	TeamName        string
	AttemptNumber   int
	StartedAt       string
	FinishedAt      string
	DurationHuman   string
	DurationSecond  int
	Score           int
	MaxScore        int
}

type quizStatsAnswerRow struct {
	SessionID          int64
	ParticipantName    string
	TeamName           string
	AttemptNumber      int
	QuestionIndex      int
	QuestionID         int
	QuestionText       string
	QuestionType       string
	SelectedOptionID   int
	SelectedOptionText string
	AnswerText         string
	IsCorrectKnown     bool
	IsCorrect          bool
}

type quizStatsReportData struct {
	Username        string
	Quiz            quiz
	Summary         quizStatsSummary
	Participants    []quizStatsParticipantRow
	Answers         []quizStatsAnswerRow
	Questions       []quizStatsQuestion
	TeamLeaderboard []TeamStanding
}

type quizStatsLoadError struct {
	Status  int
	Message string
	Err     error
}

type xlsxCell struct {
	Value any
	Style int
}

type xlsxSheet struct {
	Name       string
	Rows       [][]xlsxCell
	Widths     []float64
	FreezePane bool
	AutoFilter bool
}
