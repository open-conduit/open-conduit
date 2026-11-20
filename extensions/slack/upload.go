package slack

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

// Uploads put a file into a Work's thread with Slack's external upload flow:
// reserve the file (files.getUploadURLExternal), send its content to the
// URL that returns, then share it in the thread (files.completeUploadExternal).
// Only the share publishes anything, so only it can be ambiguous. A file
// carries no message metadata to mark it with, so the intent record keeps
// the reserved file's ID before the share is attempted: a share whose answer
// was lost is found again by that ID among the thread's messages, and if it
// is not there the same file is shared again, which needs no content. A
// reserved file that is never shared stays invisible, so a failure before
// the share is retried with a fresh reservation.

// UploadIntent is one file to put in a thread. ID must be stable across
// retries (the call's request ID).
type UploadIntent struct {
	ID              string
	Work, Execution domain.ID
	Channel, Thread string
	Name, Title     string
	Content         []byte
}

type uploadRecord struct {
	ID          string    `json:"id"`
	Work        domain.ID `json:"work"`
	Execution   domain.ID `json:"execution,omitempty"`
	Channel     string    `json:"channel"`
	Thread      string    `json:"thread"`
	Name        string    `json:"name"`
	Title       string    `json:"title"`
	Digest      string    `json:"digest"`
	Size        int       `json:"size"`
	State       string    `json:"state"`
	File        string    `json:"file,omitempty"`
	Code        string    `json:"code,omitempty"`
	Attempts    int       `json:"attempts"`
	CreatedAt   time.Time `json:"created_at"`
	AttemptedAt time.Time `json:"attempted_at,omitempty"`
}

func uploadKey(id string) string { return "upload/" + digest(id)[:40] }

// UploadState is an upload's durable record: its state, the shared file's
// ID and a refusal code.
type UploadState struct {
	State, File, Code string
}

// LookupUpload reads an upload's durable record (State is "" when unknown).
func (p *Projector) LookupUpload(ctx context.Context, id string) (UploadState, error) {
	var rec uploadRecord
	err := p.transact(ctx, func(tx storage.Tx) error {
		rec = uploadRecord{}
		_, e := p.get(tx, uploadKey(id), &rec)
		return e
	})
	return UploadState{State: rec.State, File: rec.File, Code: rec.Code}, err
}

// Upload puts a file in a thread at most once, as observed through the
// shared file's ID. It returns the durable state: posted (or recovered),
// attempted while a share's answer is unknown, pending when nothing was
// shared and the call may be retried, or failed after a definitive refusal.
func (p *Projector) Upload(ctx context.Context, in UploadIntent) (string, error) {
	if in.ID == "" || len(in.Content) == 0 || in.Name == "" || in.Title == "" || !slackIDPattern.MatchString(in.Channel) || !tsPattern.MatchString(in.Thread) {
		return "", storage.ErrInvalid
	}
	key, sum := uploadKey(in.ID), digest(string(in.Content))
	var rec uploadRecord
	err := p.transact(ctx, func(tx storage.Tx) error {
		rec = uploadRecord{}
		found, err := p.get(tx, key, &rec)
		if err != nil {
			return err
		}
		if !found {
			rec = uploadRecord{ID: in.ID, Work: in.Work, Execution: in.Execution, Channel: in.Channel, Thread: in.Thread, Name: in.Name, Title: in.Title,
				Digest: sum, Size: len(in.Content), State: IntentPending, CreatedAt: p.o.Now().UTC()}
			return p.put(tx, key, rec)
		}
		if rec.Digest != sum || rec.Channel != in.Channel || rec.Thread != in.Thread || rec.Name != in.Name || rec.Title != in.Title {
			return ErrIntentConflict
		}
		return nil
	})
	if errors.Is(err, ErrIntentConflict) {
		// The stored record is another call's: its state is not this one's,
		// so a caller never takes it for an answer still being confirmed.
		return "", err
	}
	if err != nil || rec.State == IntentPosted || rec.State == IntentFailed {
		return rec.State, err
	}
	if rec.State == IntentAttempted {
		// The content was sent and the share was asked for: finishing it
		// needs only the file's ID.
		return p.share(ctx, key, rec)
	}
	if p.o.Settings.Projection.DryRun {
		return rec.State, nil
	}
	uploadURL, file, err := p.o.Client.UploadURL(ctx, in.Name, len(in.Content))
	var refused *APIError
	switch {
	case errors.As(err, &refused):
		// Nothing was reserved, let alone shared.
		return p.finishUpload(ctx, key, IntentFailed, "", refused.Code, false)
	case err != nil:
		return rec.State, err
	}
	if err := p.o.Client.SendUpload(ctx, uploadURL, in.Content); err != nil {
		// The reserved file is never shared; a retry reserves another.
		return rec.State, err
	}
	// The callback compares against rec as it was read, so the record it
	// moves to becomes rec only once it has committed.
	var attempted uploadRecord
	err = p.transact(ctx, func(tx storage.Tx) error {
		var cur uploadRecord
		if _, err := p.get(tx, key, &cur); err != nil {
			return err
		}
		if cur.State != rec.State || cur.Attempts != rec.Attempts {
			return storage.ErrConflict
		}
		cur.State, cur.File, cur.Attempts, cur.AttemptedAt = IntentAttempted, file, cur.Attempts+1, p.o.Now().UTC()
		attempted = cur
		return p.put(tx, key, cur)
	})
	if err != nil {
		return IntentPending, err
	}
	rec = attempted
	return p.complete(ctx, key, rec, false)
}

// share finishes an attempted upload: within SearchDelay of the attempt it
// waits; after it, the file found in the thread is posted, and a file not
// there is shared again.
func (p *Projector) share(ctx context.Context, key string, rec uploadRecord) (string, error) {
	if p.o.Now().Sub(rec.AttemptedAt) < p.o.SearchDelay {
		return rec.State, nil
	}
	if found, err := p.shared(ctx, rec); err != nil {
		return rec.State, err
	} else if found {
		return p.finishUpload(ctx, key, IntentPosted, rec.File, "", true)
	}
	if p.o.Settings.Projection.DryRun {
		return rec.State, nil
	}
	var attempted uploadRecord
	err := p.transact(ctx, func(tx storage.Tx) error {
		var cur uploadRecord
		if _, err := p.get(tx, key, &cur); err != nil {
			return err
		}
		if cur.State != rec.State || cur.Attempts != rec.Attempts {
			return storage.ErrConflict
		}
		cur.Attempts, cur.AttemptedAt = cur.Attempts+1, p.o.Now().UTC()
		attempted = cur
		return p.put(tx, key, cur)
	})
	if err != nil {
		return IntentAttempted, err
	}
	rec = attempted
	return p.complete(ctx, key, rec, true)
}

// complete asks Slack to share rec's file in its thread. again says an
// earlier share's answer was lost, so a refusal may mean it was made after
// all: the thread is read once more before the upload is called failed.
func (p *Projector) complete(ctx context.Context, key string, rec uploadRecord, again bool) (string, error) {
	err := p.o.Client.CompleteUpload(ctx, rec.File, rec.Title, rec.Channel, rec.Thread)
	var limit *RateLimitError
	var refused *APIError
	switch {
	case errors.As(err, &limit), errors.Is(err, ErrCredential):
		// Refused before Slack shared anything: the same file is shared
		// once the limit lifts, by the next call or pass.
		return IntentAttempted, err
	case errors.As(err, &refused):
		if again {
			if found, e := p.shared(ctx, rec); e != nil {
				return IntentAttempted, e
			} else if found {
				return p.finishUpload(ctx, key, IntentPosted, rec.File, "", true)
			}
		}
		return p.finishUpload(ctx, key, IntentFailed, rec.File, refused.Code, false)
	case err != nil:
		// Ambiguous: the thread decides after SearchDelay.
		return IntentAttempted, err
	}
	if p.o.AfterPost != nil {
		p.o.AfterPost(rec.ID)
	}
	return p.finishUpload(ctx, key, IntentPosted, rec.File, "", false)
}

// shared reports whether rec's file is in its thread.
func (p *Projector) shared(ctx context.Context, rec uploadRecord) (bool, error) {
	messages, err := p.since(ctx, rec.Channel, rec.Thread, TS(rec.CreatedAt.Add(-time.Minute)))
	if err != nil {
		return false, err
	}
	for _, m := range messages {
		if slices.ContainsFunc(m.Files, func(f File) bool { return f.ID == rec.File }) {
			return true, nil
		}
	}
	return false, nil
}

func (p *Projector) finishUpload(ctx context.Context, key, state, file, code string, recovered bool) (string, error) {
	err := p.transact(ctx, func(tx storage.Tx) error {
		var cur uploadRecord
		if _, err := p.get(tx, key, &cur); err != nil {
			return err
		}
		cur.State, cur.Code = state, code
		if file != "" {
			cur.File = file
		}
		return p.put(tx, key, cur)
	})
	if err != nil {
		return IntentAttempted, err
	}
	if recovered {
		return IntentRecovered, nil
	}
	return state, nil
}

// convergeUploads finishes uploads whose share's answer was lost, at most
// limit of them; done counts those it tried.
func (p *Projector) convergeUploads(ctx context.Context, limit int, res *ProjectionResult) (done int, err error) {
	var open []uploadRecord
	if err := p.transact(ctx, func(tx storage.Tx) error {
		open = nil
		rows, err := tx.List(p.ns, "upload/")
		for _, row := range rows {
			var rec uploadRecord
			if json.Unmarshal(row.Value, &rec) == nil && rec.State == IntentAttempted && rec.File != "" {
				open = append(open, rec)
			}
		}
		return err
	}); err != nil {
		return 0, err
	}
	for _, rec := range open {
		if done >= limit {
			break
		}
		done++
		state, err := p.share(ctx, uploadKey(rec.ID), rec)
		var rate *RateLimitError
		switch {
		case errors.As(err, &rate):
			res.Codes = append(res.Codes, "upload:rate_limited")
			return done, err
		case errors.Is(err, ErrCredential):
			res.Codes = append(res.Codes, "upload:credential")
			return done, err
		case err != nil:
			res.Codes = append(res.Codes, "upload:"+ErrorCode(err))
		case state == IntentRecovered:
			res.Recovered++
		case state == IntentPosted:
			res.Posted++
		case state == IntentFailed:
			res.Failed++
			res.Codes = append(res.Codes, "upload:refused")
		}
	}
	return done, nil
}
