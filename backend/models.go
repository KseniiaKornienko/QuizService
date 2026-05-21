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
