package main

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

const avatarTTL = 3 * 24 * time.Hour

// Profile picture lookups are rate limited by WhatsApp; a few at a time is plenty for a scrolling list.
var avatarSlots = make(chan struct{}, 4)

func (a *App) apiAvatar(w http.ResponseWriter, r *http.Request) {
	jid, err := types.ParseJID(r.URL.Query().Get("jid"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	base := filepath.Join(a.dataDir, "avatars", unsafeChars.ReplaceAllString(jid.String(), "_"))
	img, none := base+".jpg", base+".none"

	fresh := func(p string) bool {
		st, err := os.Stat(p)
		return err == nil && time.Since(st.ModTime()) < avatarTTL
	}
	if fresh(none) {
		http.NotFound(w, r)
		return
	}
	if !fresh(img) {
		if err := a.fetchAvatar(r, jid, img, none); err != nil {
			if _, statErr := os.Stat(img); statErr != nil { // serve a stale copy if we have one
				http.NotFound(w, r)
				return
			}
		}
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeFile(w, r, img)
}

func (a *App) fetchAvatar(r *http.Request, jid types.JID, img, none string) error {
	select {
	case avatarSlots <- struct{}{}:
		defer func() { <-avatarSlots }()
	case <-r.Context().Done(): // scrolled past before it was our turn
		return r.Context().Err()
	}
	os.MkdirAll(filepath.Dir(img), 0o755)
	info, err := a.cli.GetProfilePictureInfo(a.ctx, jid, &whatsmeow.GetProfilePictureParams{Preview: true})
	if errors.Is(err, whatsmeow.ErrProfilePictureNotSet) || errors.Is(err, whatsmeow.ErrProfilePictureUnauthorized) || (err == nil && info == nil) {
		os.Remove(img)
		return os.WriteFile(none, nil, 0o644)
	}
	if err != nil {
		return err
	}
	resp, err := http.Get(info.URL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New(resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return err
	}
	os.Remove(none)
	return os.WriteFile(img, data, 0o644)
}
