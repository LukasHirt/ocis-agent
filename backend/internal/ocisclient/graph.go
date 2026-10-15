package ocisclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

// User is the Graph view of the calling user. Username is the login name that
// app-token Basic auth needs.
type User struct {
	ID          string
	Username    string
	DisplayName string
}

// Me calls GET /graph/v1.0/me. It doubles as the cheap "does this credential
// work" probe.
func (c *Client) Me(ctx context.Context, cr Creds) (User, error) {
	const p = "/graph/v1.0/me"
	resp, err := c.do(ctx, cr, http.MethodGet, p, nil, nil, nil, maxBody)
	if err != nil {
		return User{}, err
	}
	if resp.status != http.StatusOK {
		return User{}, statusError(http.MethodGet, p, resp.status, nil)
	}
	var out struct {
		ID          string `json:"id"`
		DisplayName string `json:"displayName"`
		Username    string `json:"onPremisesSamAccountName"`
	}
	if err := json.Unmarshal(resp.body, &out); err != nil || out.ID == "" {
		return User{}, errors.New("ocisclient: GET " + p + ": no user id in the response")
	}
	return User{ID: out.ID, Username: out.Username, DisplayName: out.DisplayName}, nil
}

// PersonalDriveID returns the id of the user's personal space.
func (c *Client) PersonalDriveID(ctx context.Context, cr Creds) (string, error) {
	const p = "/graph/v1.0/me/drives"
	resp, err := c.do(ctx, cr, http.MethodGet, p, nil, nil, nil, maxBody)
	if err != nil {
		return "", err
	}
	if resp.status != http.StatusOK {
		return "", statusError(http.MethodGet, p, resp.status, nil)
	}
	var out struct {
		Value []struct {
			ID        string `json:"id"`
			DriveType string `json:"driveType"`
		} `json:"value"`
	}
	if err := json.Unmarshal(resp.body, &out); err != nil {
		return "", errors.New("ocisclient: GET " + p + ": malformed response")
	}
	for _, d := range out.Value {
		if d.DriveType == "personal" && d.ID != "" {
			return d.ID, nil
		}
	}
	return "", ErrNoPersonalDrive
}
