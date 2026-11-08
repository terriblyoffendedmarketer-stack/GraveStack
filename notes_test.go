// notes_test.go — Unit tests for note creation, editing, and deletion.
// Run with: go test -v -run TestNotes ./...

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestNotesCRUD(t *testing.T) {
	db := testDB(t)
	artID := seedArticle(t, db, "art-notes", "Note Test", 1)

	srv := &server{db: db, cfg: Config{}, auth: newAuth(db, "")}
	mux := http.NewServeMux()
	srv.routes(mux)

	// 1. Create note
	body, _ := json.Marshal(map[string]string{"text": "Initial note content"})
	req := httptest.NewRequest("POST", "/api/article/"+strconv.FormatInt(artID, 10)+"/notes", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on save, got %d: %s", w.Code, w.Body.String())
	}

	var saveResp struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &saveResp); err != nil || saveResp.ID == 0 {
		t.Fatalf("invalid save response: %s", w.Body.String())
	}
	noteID := saveResp.ID

	// 2. Update note (PUT)
	updateBody, _ := json.Marshal(map[string]string{"text": "Updated note content"})
	reqUpdate := httptest.NewRequest("PUT", "/api/note/"+strconv.FormatInt(noteID, 10), bytes.NewReader(updateBody))
	reqUpdate.Header.Set("Content-Type", "application/json")
	wUpdate := httptest.NewRecorder()
	mux.ServeHTTP(wUpdate, reqUpdate)

	if wUpdate.Code != http.StatusOK {
		t.Fatalf("expected 200 on update, got %d: %s", wUpdate.Code, wUpdate.Body.String())
	}

	// Verify update in list
	reqList := httptest.NewRequest("GET", "/api/article/"+strconv.FormatInt(artID, 10)+"/notes", nil)
	wList := httptest.NewRecorder()
	mux.ServeHTTP(wList, reqList)

	var notes []struct {
		ID   int64  `json:"id"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(wList.Body.Bytes(), &notes); err != nil || len(notes) != 1 {
		t.Fatalf("failed to list notes: %s", wList.Body.String())
	}
	if notes[0].Text != "Updated note content" {
		t.Errorf("expected updated text, got %q", notes[0].Text)
	}

	// 3. Delete note
	reqDel := httptest.NewRequest("DELETE", "/api/note/"+strconv.FormatInt(noteID, 10), nil)
	wDel := httptest.NewRecorder()
	mux.ServeHTTP(wDel, reqDel)

	if wDel.Code != http.StatusOK {
		t.Fatalf("expected 200 on delete, got %d: %s", wDel.Code, wDel.Body.String())
	}

	// Verify deletion
	wList2 := httptest.NewRecorder()
	mux.ServeHTTP(wList2, reqList)
	var notesAfter []any
	json.Unmarshal(wList2.Body.Bytes(), &notesAfter)
	if len(notesAfter) != 0 {
		t.Errorf("expected 0 notes after delete, got %d", len(notesAfter))
	}
}
