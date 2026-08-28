package event

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/patrickyoung/ask/internal/provider"
)

// EvidenceData indexes a Context JSONL snapshot already present in the user
// message. Block and Offset point to the exact bytes the provider saw; the
// snapshot is not duplicated in the log. Check reconstructs this manifest on
// replay, so its digest and provenance cannot drift away from those bytes.
type EvidenceData struct {
	Format        string           `json:"format"`
	Block         int              `json:"block"`
	Offset        int              `json:"offset"`
	SnapshotBytes int              `json:"snapshot_bytes"`
	SHA256        string           `json:"sha256"`
	Records       []EvidenceRecord `json:"records"`
}

// EvidenceRecord is the ordered provenance index for one Context record.
// Content remains only in the snapshot; the index keeps review and search
// cheap without creating a second copy of potentially large evidence.
type EvidenceRecord struct {
	Ref             string `json:"ref"`
	Source          string `json:"source"`
	ID              string `json:"id"`
	RetrievedAt     string `json:"retrieved_at"`
	CitationLocator string `json:"citation_locator"`
	CitationURL     string `json:"citation_url,omitempty"`
	Query           string `json:"query,omitempty"`
	Connector       string `json:"connector,omitempty"`
	ConnectorSHA256 string `json:"connector_sha256,omitempty"`
}

type contextWire struct {
	Kind        string          `json:"kind"`
	Version     int             `json:"version"`
	Source      string          `json:"source"`
	ID          string          `json:"id"`
	Type        string          `json:"type"`
	Title       string          `json:"title"`
	Ref         string          `json:"ref"`
	RetrievedAt string          `json:"retrieved_at"`
	Content     json.RawMessage `json:"content"`
	Citation    struct {
		Locator string `json:"locator"`
		URL     string `json:"url"`
	} `json:"citation"`
	Retrieval *struct {
		Query     string `json:"query"`
		Connector struct {
			Name   string `json:"name"`
			SHA256 string `json:"sha256"`
		} `json:"connector"`
	} `json:"retrieval"`
}

// ContextEvidence recognizes normalized context/v1 JSONL. The bool is false
// for ordinary textual stdin. Once the first record claims the Context wire
// kind, every record is checked: damaged evidence must not silently become an
// unlabelled prompt.
func ContextEvidence(snapshot []byte, block, offset int) (*EvidenceData, bool, error) {
	lines := bytes.Split(snapshot, []byte("\n"))
	if len(lines) == 0 || len(bytes.TrimSpace(lines[0])) == 0 {
		return nil, false, nil
	}
	var probe struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(lines[0], &probe); err != nil || probe.Kind != "context" {
		return nil, false, nil
	}

	manifest := &EvidenceData{
		Format:        "context/v1",
		Block:         block,
		Offset:        offset,
		SnapshotBytes: len(snapshot),
		SHA256:        evidenceDigest(snapshot),
	}
	scanner := bufio.NewScanner(bytes.NewReader(snapshot))
	scanner.Buffer(make([]byte, 64*1024), len(snapshot)+1)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			return nil, true, fmt.Errorf("context evidence line %d is blank", lineNo)
		}
		var row contextWire
		if err := json.Unmarshal(line, &row); err != nil {
			return nil, true, fmt.Errorf("context evidence line %d: invalid JSON: %w", lineNo, err)
		}
		if row.Kind != "context" || row.Version != 1 {
			return nil, true, fmt.Errorf("context evidence line %d must be context/v1", lineNo)
		}
		if row.Source == "" || row.ID == "" || row.Type == "" || row.Title == "" ||
			row.Ref == "" || row.Citation.Locator == "" || len(row.Content) == 0 ||
			bytes.Equal(bytes.TrimSpace(row.Content), []byte("null")) {
			return nil, true, fmt.Errorf("context evidence line %d lacks a required context/v1 field", lineNo)
		}
		if _, err := time.Parse(time.RFC3339, row.RetrievedAt); err != nil {
			return nil, true, fmt.Errorf("context evidence line %d has invalid retrieved_at", lineNo)
		}
		record := EvidenceRecord{
			Ref: row.Ref, Source: row.Source, ID: row.ID,
			RetrievedAt: row.RetrievedAt, CitationLocator: row.Citation.Locator,
			CitationURL: row.Citation.URL,
		}
		if row.Retrieval != nil {
			if strings.TrimSpace(row.Retrieval.Query) == "" || row.Retrieval.Connector.Name == "" ||
				!validEvidenceDigest(row.Retrieval.Connector.SHA256) {
				return nil, true, fmt.Errorf("context evidence line %d has invalid retrieval provenance", lineNo)
			}
			record.Query = row.Retrieval.Query
			record.Connector = row.Retrieval.Connector.Name
			record.ConnectorSHA256 = row.Retrieval.Connector.SHA256
		}
		manifest.Records = append(manifest.Records, record)
	}
	if err := scanner.Err(); err != nil {
		return nil, true, fmt.Errorf("context evidence: %w", err)
	}
	if len(manifest.Records) == 0 {
		return nil, true, fmt.Errorf("context evidence has no records")
	}
	return manifest, true, nil
}

func evidenceDigest(snapshot []byte) string {
	sum := sha256.Sum256(snapshot)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func validEvidenceDigest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

// CheckEvidence proves that the manifest describes the exact Context bytes
// carried in this message. Old and ordinary user events have no manifest.
func (u UserData) CheckEvidence() error {
	if u.Evidence == nil {
		return nil
	}
	blocks := u.Content()
	if u.Evidence.Block < 0 || u.Evidence.Block >= len(blocks) {
		return fmt.Errorf("evidence block %d is outside user message", u.Evidence.Block)
	}
	b := blocks[u.Evidence.Block]
	if b.Type != provider.Text || u.Evidence.Offset < 0 || u.Evidence.SnapshotBytes < 0 ||
		u.Evidence.Offset > len(b.Text) || u.Evidence.SnapshotBytes > len(b.Text)-u.Evidence.Offset {
		return fmt.Errorf("evidence location is outside user text block %d", u.Evidence.Block)
	}
	snapshot := []byte(b.Text[u.Evidence.Offset : u.Evidence.Offset+u.Evidence.SnapshotBytes])
	got, claimed, err := ContextEvidence(snapshot, u.Evidence.Block, u.Evidence.Offset)
	if err != nil {
		return err
	}
	if !claimed || !reflect.DeepEqual(got, u.Evidence) {
		return fmt.Errorf("evidence manifest diverges from user message snapshot")
	}
	return nil
}
