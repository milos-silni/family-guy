package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/jwt"
)

// Push notifications to the Android app go through Firebase Cloud Messaging
// (HTTP v1 API). They are on when -fcm-credentials points at the JSON key of
// a Firebase service account; without it the app can't register for them.

type fcmSender struct {
	client  *http.Client // signs requests with the service account's OAuth token
	sendURL string
}

func loadFCM(path string) (*fcmSender, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var key struct {
		Type        string `json:"type"`
		ProjectID   string `json:"project_id"`
		ClientEmail string `json:"client_email"`
		PrivateKey  string `json:"private_key"`
		TokenURI    string `json:"token_uri"`
	}
	if err := json.Unmarshal(b, &key); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if key.Type != "service_account" || key.ProjectID == "" || key.ClientEmail == "" || key.PrivateKey == "" {
		return nil, fmt.Errorf("%s is not a Firebase service account key", path)
	}
	if key.TokenURI == "" {
		key.TokenURI = "https://oauth2.googleapis.com/token"
	}
	conf := &jwt.Config{
		Email:      key.ClientEmail,
		PrivateKey: []byte(key.PrivateKey),
		Scopes:     []string{"https://www.googleapis.com/auth/firebase.messaging"},
		TokenURL:   key.TokenURI,
	}
	client := conf.Client(context.WithValue(context.Background(), oauth2.HTTPClient, pushClient))
	client.Timeout = pushClient.Timeout
	return &fcmSender{
		client:  client,
		sendURL: "https://fcm.googleapis.com/v1/projects/" + key.ProjectID + "/messages:send",
	}, nil
}

// sendFCM delivers msg to every Android install of a user and returns how
// many FCM accepted. Tokens FCM no longer knows are forgotten.
func (a *App) sendFCM(userID int64, msg pushMessage) (int, error) {
	if a.fcm == nil {
		return 0, nil
	}
	rows, err := a.db.Query(`SELECT id, token FROM fcm_tokens WHERE user_id = ?`, userID)
	if err != nil {
		return 0, err
	}
	type device struct {
		id    int64
		token string
	}
	var devices []device
	for rows.Next() {
		var d device
		if err := rows.Scan(&d.id, &d.token); err != nil {
			rows.Close()
			return 0, err
		}
		devices = append(devices, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	sent := 0
	for _, d := range devices {
		gone, err := a.fcm.send(d.token, msg)
		switch {
		case gone:
			a.db.Exec(`DELETE FROM fcm_tokens WHERE id = ?`, d.id)
		case err != nil:
			log.Printf("fcm: %v", err)
		default:
			sent++
		}
	}
	return sent, nil
}

// send posts one message. gone means FCM says the token is no longer valid.
func (f *fcmSender) send(token string, msg pushMessage) (gone bool, err error) {
	body, _ := json.Marshal(map[string]any{
		"message": map[string]any{
			"token":        token,
			"notification": map[string]string{"title": msg.Title, "body": msg.Body},
			"data":         map[string]string{"type": "shopping_list", "url": msg.URL},
			"android": map[string]any{
				"priority":     "HIGH",
				"ttl":          "43200s",
				"collapse_key": "shopping", // a newer ping replaces an undelivered older one
				"notification": map[string]string{"channel_id": "shopping", "tag": "shopping"},
			},
		},
	})
	resp, err := f.client.Post(f.sendURL, "application/json", bytes.NewReader(body))
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) {
			return false, fmt.Errorf("service account login failed: %s", re.Response.Status)
		}
		return false, err
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	switch {
	case resp.StatusCode == http.StatusOK:
		return false, nil
	case resp.StatusCode == http.StatusNotFound || bytes.Contains(answer, []byte("UNREGISTERED")):
		return true, nil
	default:
		return false, fmt.Errorf("FCM answered %s: %s", resp.Status, strings.TrimSpace(string(answer)))
	}
}
