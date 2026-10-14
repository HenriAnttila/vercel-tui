package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type projectLink struct {
	ProjectID string `json:"projectId"`
	OrgID     string `json:"orgId"`
}

func (l projectLink) teamID() string {
	if strings.HasPrefix(l.OrgID, "team_") {
		return l.OrgID
	}
	return ""
}

func findProjectLink(start string) (projectLink, string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return projectLink{}, "", err
	}
	for {
		path := filepath.Join(dir, ".vercel", "project.json")
		data, err := os.ReadFile(path)
		if err == nil {
			var link projectLink
			if err := json.Unmarshal(data, &link); err != nil {
				return projectLink{}, "", fmt.Errorf("%s: %w", path, err)
			}
			if link.ProjectID == "" {
				return projectLink{}, "", fmt.Errorf("%s has no projectId", path)
			}
			return link, dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return projectLink{}, "", errors.New("no .vercel/project.json found here or in any parent; run `vercel link` first")
		}
		dir = parent
	}
}

func loadToken() (string, error) {
	if token := os.Getenv("VERCEL_TOKEN"); token != "" {
		return token, nil
	}
	for _, path := range authPaths() {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var auth struct {
			Token string `json:"token"`
		}
		if json.Unmarshal(data, &auth) == nil && auth.Token != "" {
			return auth.Token, nil
		}
	}
	return "", errors.New("no Vercel token found; set VERCEL_TOKEN or run `vercel login`")
}

func authPaths() []string {
	home, _ := os.UserHomeDir()
	var paths []string
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		paths = append(paths, filepath.Join(xdg, "com.vercel.cli", "auth.json"))
	}
	return append(paths,
		filepath.Join(home, "Library", "Application Support", "com.vercel.cli", "auth.json"),
		filepath.Join(home, ".local", "share", "com.vercel.cli", "auth.json"),
	)
}
