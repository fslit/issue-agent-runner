package prompt

import (
	"bytes"
	"os"
	"text/template"
)

type Data struct {
	RepoURL       string
	Number        string
	Title         string
	Body          string
	ResultPath    string
	CommandPrefix string
	Repositories  []RepositoryData
}

type RepositoryData struct {
	Name           string
	Path           string
	WorktreePath   string
	IssueBranch    string
	BaselineBranch string
}

func RenderFile(path string, data Data) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return RenderText(string(raw), data)
}

func RenderText(text string, data Data) (string, error) {
	tmpl, err := template.New("worker_prompt").Option("missingkey=error").Parse(text)
	if err != nil {
		return "", err
	}

	values := map[string]any{
		"RepoURL":       data.RepoURL,
		"Number":        data.Number,
		"Title":         data.Title,
		"Body":          data.Body,
		"ResultPath":    data.ResultPath,
		"CommandPrefix": data.CommandPrefix,
		"Repositories":  data.Repositories,
	}

	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, values); err != nil {
		return "", err
	}
	return rendered.String(), nil
}
