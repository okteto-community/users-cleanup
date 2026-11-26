package okteto

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestListUsers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v0/users" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("unexpected request: %s %s %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		_, _ = io.WriteString(w, `[
			{"id":"1","name":"Ada","email":"ada@example.com","role":"Admin","defaultNamespace":"ada","creationDate":"2025-01-02T03:04:05Z","lastSeen":"2026-11-01T10:00:00Z"},
			{"id":"2","name":"Bob","email":"bob@example.com","role":"Developer","defaultNamespace":"bob"}
		]`)
	}))
	defer srv.Close()

	users, err := New(srv.URL+"/", "secret").ListUsers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 || users[0].Role != "Admin" || users[0].LastSeen == nil || users[0].LastSeen.Day() != 1 {
		t.Errorf("unexpected users: %+v", users)
	}
	if users[1].LastSeen != nil || users[1].CreationDate != nil {
		t.Errorf("missing dates must stay empty: %+v", users[1])
	}
}

func TestListUsersWithoutAdminToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"forbidden"}`)
	}))
	defer srv.Close()

	if _, err := New(srv.URL, "x").ListUsers(context.Background()); !errors.Is(err, ErrNotAdmin) {
		t.Errorf("got %v, want ErrNotAdmin", err)
	}
}

func TestDeleteUser(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("unexpected method %s", r.Method)
		}
		switch r.URL.Path {
		case "/api/v0/users/ok":
			w.WriteHeader(http.StatusNoContent)
		case "/api/v0/users/self":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"message":"Cannot delete your own account"}`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "x")

	if err := c.DeleteUser(context.Background(), "ok"); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if err := c.DeleteUser(context.Background(), "self"); err == nil || !strings.Contains(err.Error(), "Cannot delete your own account") {
		t.Errorf("the message of the API must be reported: %v", err)
	}
	if err := c.DeleteUser(context.Background(), "other"); err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("got %v", err)
	}
}

func TestSetPersistent(t *testing.T) {
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query     string            `json:"query"`
			Variables map[string]string `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || r.URL.Path != "/graphql" {
			t.Errorf("unexpected request: %s %v", r.URL.Path, err)
		}
		queries = append(queries, body.Query)
		if body.Variables["space"] == "denied" {
			_, _ = io.WriteString(w, `{"data":null,"errors":[{"message":"not-authorized"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":{"enablePersistentSpace":{"id":"ns"}}}`)
	}))
	defer srv.Close()
	c := New(srv.URL, "x")

	if err := c.SetPersistent(context.Background(), "ns", true); err != nil {
		t.Fatal(err)
	}
	if err := c.SetPersistent(context.Background(), "ns", false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(queries[0], "enablePersistentSpace") || !strings.Contains(queries[1], "disablePersistentSpace") {
		t.Errorf("unexpected mutations: %v", queries)
	}
	if err := c.SetPersistent(context.Background(), "denied", true); err == nil || err.Error() != "not-authorized" {
		t.Errorf("GraphQL errors must be reported: %v", err)
	}
}
