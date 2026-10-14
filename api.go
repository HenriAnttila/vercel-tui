package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const apiBase = "https://api.vercel.com"

type client struct {
	token  string
	teamID string
	http   *http.Client
}

func newClient(token, teamID string) *client {
	return &client{token: token, teamID: teamID, http: &http.Client{Timeout: 30 * time.Second}}
}

type apiError struct {
	Status  int
	Code    string
	Message string
}

func (e *apiError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = e.Code
	}
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	return fmt.Sprintf("%d: %s", e.Status, msg)
}

func (c *client) do(ctx context.Context, method, path string, query url.Values, body, out any) (int, error) {
	if query == nil {
		query = url.Values{}
	}
	if c.teamID != "" {
		query.Set("teamId", c.teamID)
	}
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, apiBase+path+"?"+query.Encode(), reader)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		return res.StatusCode, err
	}
	if res.StatusCode >= 300 {
		var e struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		return res.StatusCode, &apiError{Status: res.StatusCode, Code: e.Error.Code, Message: e.Error.Message}
	}
	if out != nil && len(data) > 0 {
		return res.StatusCode, json.Unmarshal(data, out)
	}
	return res.StatusCode, nil
}

type project struct {
	ID                      string `json:"id"`
	Name                    string `json:"name"`
	AutoAssignCustomDomains *bool  `json:"autoAssignCustomDomains"`
	Link                    struct {
		ProductionBranch string `json:"productionBranch"`
	} `json:"link"`
	Targets struct {
		Production *struct {
			ID    string   `json:"id"`
			Alias []string `json:"alias"`
		} `json:"production"`
	} `json:"targets"`
}

func (p project) autoAssign() bool {
	return p.AutoAssignCustomDomains == nil || *p.AutoAssignCustomDomains
}

func (p project) productionID() string {
	if p.Targets.Production == nil {
		return ""
	}
	return p.Targets.Production.ID
}

func (p project) productionDomain() string {
	if p.Targets.Production == nil || len(p.Targets.Production.Alias) == 0 {
		return ""
	}
	for _, alias := range p.Targets.Production.Alias {
		if !strings.HasSuffix(alias, ".vercel.app") {
			return alias
		}
	}
	return p.Targets.Production.Alias[0]
}

type deployment struct {
	UID                 string            `json:"uid"`
	URL                 string            `json:"url"`
	State               string            `json:"state"`
	ReadySubstate       string            `json:"readySubstate"`
	Target              string            `json:"target"`
	Source              string            `json:"source"`
	Created             int64             `json:"created"`
	BuildingAt          int64             `json:"buildingAt"`
	Ready               int64             `json:"ready"`
	InspectorURL        string            `json:"inspectorUrl"`
	IsRollbackCandidate bool              `json:"isRollbackCandidate"`
	Meta                map[string]string `json:"meta"`
	Creator             struct {
		Username string `json:"username"`
	} `json:"creator"`
}

func (d deployment) active() bool {
	switch d.State {
	case "QUEUED", "INITIALIZING", "BUILDING":
		return true
	}
	return false
}

func (d deployment) production() bool {
	return d.Target == "production"
}

func (d deployment) gitMeta(key string) string {
	for _, provider := range []string{"github", "gitlab", "bitbucket"} {
		if v := d.Meta[provider+key]; v != "" {
			return v
		}
	}
	return ""
}

func (d deployment) branch() string {
	return d.gitMeta("CommitRef")
}

func (d deployment) sha() string {
	sha := d.gitMeta("CommitSha")
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func (d deployment) message() string {
	msg, _, _ := strings.Cut(d.gitMeta("CommitMessage"), "\n")
	if msg == "" && d.Source != "" {
		return "(" + d.Source + " deploy)"
	}
	return msg
}

func (d deployment) author() string {
	if v := d.gitMeta("CommitAuthorLogin"); v != "" {
		return v
	}
	if v := d.gitMeta("CommitAuthorName"); v != "" {
		return v
	}
	return d.Creator.Username
}

func (d deployment) short() string {
	id := strings.TrimPrefix(d.UID, "dpl_")
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

type event struct {
	Created int64  `json:"created"`
	Type    string `json:"type"`
	Text    string `json:"text"`
}

func (c *client) project(ctx context.Context, id string) (project, error) {
	var p project
	_, err := c.do(ctx, http.MethodGet, "/v9/projects/"+url.PathEscape(id), nil, nil, &p)
	return p, err
}

func (c *client) scopeName(ctx context.Context) (string, error) {
	if c.teamID == "" {
		var u struct {
			User struct {
				Username string `json:"username"`
			} `json:"user"`
		}
		_, err := c.do(ctx, http.MethodGet, "/v2/user", nil, nil, &u)
		return u.User.Username, err
	}
	var t struct {
		Slug string `json:"slug"`
	}
	_, err := c.do(ctx, http.MethodGet, "/v2/teams/"+url.PathEscape(c.teamID), nil, nil, &t)
	return t.Slug, err
}

func (c *client) deployments(ctx context.Context, projectID, target string, limit int) ([]deployment, error) {
	query := url.Values{"projectId": {projectID}, "limit": {strconv.Itoa(limit)}}
	if target != "" {
		query.Set("target", target)
	}
	var r struct {
		Deployments []deployment `json:"deployments"`
	}
	_, err := c.do(ctx, http.MethodGet, "/v6/deployments", query, nil, &r)
	return r.Deployments, err
}

func (c *client) events(ctx context.Context, id string) ([]event, error) {
	query := url.Values{"builds": {"1"}, "direction": {"forward"}, "limit": {"-1"}}
	var evs []event
	_, err := c.do(ctx, http.MethodGet, "/v3/deployments/"+url.PathEscape(id)+"/events", query, nil, &evs)
	return evs, err
}

func (c *client) cancel(ctx context.Context, id string) error {
	_, err := c.do(ctx, http.MethodPatch, "/v12/deployments/"+url.PathEscape(id)+"/cancel", nil, nil, nil)
	return err
}

func (c *client) redeploy(ctx context.Context, p project, fromID, target string) (string, error) {
	body := map[string]string{"name": p.Name, "project": p.ID, "deploymentId": fromID}
	if target != "" {
		body["target"] = target
	}
	var r struct {
		URL string `json:"url"`
	}
	_, err := c.do(ctx, http.MethodPost, "/v13/deployments", url.Values{"forceNew": {"1"}}, body, &r)
	return r.URL, err
}

func (c *client) promote(ctx context.Context, projectID, id string) (bool, error) {
	status, err := c.do(ctx, http.MethodPost, "/v10/projects/"+url.PathEscape(projectID)+"/promote/"+url.PathEscape(id), nil, nil, nil)
	return status == http.StatusAccepted, err
}

func (c *client) rollback(ctx context.Context, projectID, id string) error {
	_, err := c.do(ctx, http.MethodPost, "/v1/projects/"+url.PathEscape(projectID)+"/rollback/"+url.PathEscape(id), nil, nil, nil)
	return err
}

type requestRecord struct {
	Timestamp     string `json:"timestamp"`
	RequestID     string `json:"requestId"`
	RequestPath   string `json:"requestPath"`
	RequestMethod string `json:"requestMethod"`
	HTTPStatus    int    `json:"httpStatus"`
	PathType      string `json:"pathType"`
	CacheResult   string `json:"cacheResult"`
}

type logLine struct {
	Timestamp     string `json:"timestamp"`
	RequestID     string `json:"requestId"`
	RequestPath   string `json:"requestPath"`
	RequestMethod string `json:"requestMethod"`
	Level         string `json:"level"`
	Message       string `json:"message"`
	Truncated     bool   `json:"truncated"`
	at            time.Time
}

type eventsQuery struct {
	Event     string      `json:"event"`
	Scope     eventsScope `json:"scope"`
	TimeRange eventsRange `json:"timeRange"`
	Select    []string    `json:"select"`
	Filter    string      `json:"filter,omitempty"`
	Order     string      `json:"order"`
	Limit     int         `json:"limit"`
}

type eventsScope struct {
	OwnerID    string   `json:"ownerId"`
	ProjectIDs []string `json:"projectIds"`
}

type eventsRange struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

func isRetentionError(err error) bool {
	var apiErr *apiError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusBadRequest && strings.Contains(apiErr.Message, "retain")
}

func (c *client) runtime(ctx context.Context, ownerID, projectID, deployID string, start, end time.Time, limit int) ([]requestRecord, []logLine, error) {
	base := eventsQuery{
		Scope:     eventsScope{OwnerID: ownerID, ProjectIDs: []string{projectID}},
		TimeRange: eventsRange{Start: start.UTC().Format("2006-01-02T15:04:05.000Z"), End: end.UTC().Format("2006-01-02T15:04:05.000Z")},
		Order:     "desc",
		Limit:     limit,
	}
	deployFilter := fmt.Sprintf("(deploymentId:%q)", deployID)

	reqQuery := base
	reqQuery.Event = "vercel.proxy_response"
	reqQuery.Select = []string{"requestId", "requestPath", "requestMethod", "httpStatus", "pathType", "cacheResult"}
	reqQuery.Filter = deployFilter + ` AND (NOT pathType:"background_func")`

	logQuery := base
	logQuery.Event = "vercel.log"
	logQuery.Select = []string{"requestId", "requestPath", "requestMethod", "level", "message", "truncated"}
	logQuery.Filter = deployFilter

	var (
		reqs           struct{ Events []requestRecord }
		logs           struct{ Events []logLine }
		reqErr, logErr error
		wg             sync.WaitGroup
	)
	wg.Go(func() {
		_, reqErr = c.do(ctx, http.MethodPost, "/v1/observability/events", url.Values{"teamId": {ownerID}}, reqQuery, &reqs)
	})
	wg.Go(func() {
		_, logErr = c.do(ctx, http.MethodPost, "/v1/observability/events", url.Values{"teamId": {ownerID}}, logQuery, &logs)
	})
	wg.Wait()
	return reqs.Events, logs.Events, errors.Join(reqErr, logErr)
}
