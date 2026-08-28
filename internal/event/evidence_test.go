package event

import (
	"encoding/json"
	"strings"
	"testing"
)

const contextSnapshot = `{"kind":"context","version":1,"source":"handbook","type":"document","id":"leave-7","title":"Paid leave","retrieved_at":"2026-08-28T12:00:00Z","content":{"text":"Twenty days"},"citation":{"locator":"handbook.md#leave","url":"https://example.test/leave"},"ref":"ctx:handbook:abc","retrieval":{"query":"paid leave?","connector":{"name":"handbook","sha256":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}}`

func TestContextEvidenceIndexesSnapshotWithoutDuplicatingContent(t *testing.T) {
	manifest, claimed, err := ContextEvidence([]byte(contextSnapshot), 2, 17)
	if err != nil || !claimed {
		t.Fatalf("ContextEvidence: claimed=%v err=%v", claimed, err)
	}
	if manifest.Format != "context/v1" || manifest.Block != 2 || manifest.Offset != 17 || len(manifest.Records) != 1 {
		t.Fatalf("manifest = %+v", manifest)
	}
	r := manifest.Records[0]
	if r.Ref != "ctx:handbook:abc" || r.Query != "paid leave?" || r.Connector != "handbook" ||
		r.CitationURL != "https://example.test/leave" {
		t.Fatalf("record = %+v", r)
	}
	raw, _ := json.Marshal(manifest)
	if strings.Contains(string(raw), "Twenty days") {
		t.Fatal("manifest duplicated evidence content")
	}
}

func TestEvidenceManifestIsReplayCheckedAgainstMessage(t *testing.T) {
	prefix := "Question\n\n<stdin>\n"
	text := prefix + contextSnapshot + "\n</stdin>"
	manifest, _, err := ContextEvidence([]byte(contextSnapshot), 0, len(prefix))
	if err != nil {
		t.Fatal(err)
	}
	u := UserData{Text: text, Evidence: manifest}
	if err := u.CheckEvidence(); err != nil {
		t.Fatalf("valid evidence: %v", err)
	}

	u.Evidence.Records[0].Ref = "ctx:handbook:changed"
	if err := u.CheckEvidence(); err == nil || !strings.Contains(err.Error(), "diverges") {
		t.Fatalf("changed manifest check = %v", err)
	}

	manifest, _, _ = ContextEvidence([]byte(contextSnapshot), 0, len(prefix))
	u = UserData{Text: strings.Replace(text, "Twenty days", "Thirty days", 1), Evidence: manifest}
	raw, _ := json.Marshal(u)
	if _, err := Fold([]Event{{Seq: 1, Type: User, Data: raw}}); err == nil || !strings.Contains(err.Error(), "diverges") {
		t.Fatalf("changed snapshot fold = %v", err)
	}
	if err := Check([]Event{{Seq: 1, Type: User, Data: raw}}); err == nil || !strings.Contains(err.Error(), "diverges") {
		t.Fatalf("changed terminal snapshot check = %v", err)
	}
}

func TestContextEvidenceRefusesDamagedClaimedStream(t *testing.T) {
	_, claimed, err := ContextEvidence([]byte(contextSnapshot+"\n{bad"), 0, 0)
	if !claimed || err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("claimed=%v err=%v", claimed, err)
	}
	if got, claimed, err := ContextEvidence([]byte("ordinary stdin"), 0, 0); got != nil || claimed || err != nil {
		t.Fatalf("ordinary stdin = %+v, %v, %v", got, claimed, err)
	}
}
