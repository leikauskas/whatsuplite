package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows/registry"
)

// transcribe turns a voice note into text and saves it on the message, so the window, search
// and agents all see it. ElevenLabs Scribe handles Lithuanian and English with language detection;
// the local Parakeet model is the fallback when Scribe is unavailable.
func (a *App) transcribe(m *Msg) (string, error) {
	if m.Text != "" {
		return m.Text, nil
	}
	path, err := a.mediaFile(m)
	if err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	text, err := scribe(path)
	if err != nil {
		logf("transcribe: scribe failed (%v), trying parakeet", err)
		if text, err = parakeet(path); err != nil {
			return "", err
		}
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", errors.New("no speech found")
	}
	a.db.SetText(m.Chat, m.ID, text)
	a.pushMsg(m.Chat, m.ID)
	return text, nil
}

func elevenKey() string {
	if k := os.Getenv("ELEVENLABS_API_KEY"); k != "" {
		return k
	}
	// A key added after login isn't in this process's environment yet; the registry has it.
	if k, err := registry.OpenKey(registry.CURRENT_USER, `Environment`, registry.QUERY_VALUE); err == nil {
		defer k.Close()
		if v, _, err := k.GetStringValue("ELEVENLABS_API_KEY"); err == nil {
			return v
		}
	}
	return ""
}

func scribe(path string) (string, error) {
	key := elevenKey()
	if key == "" {
		return "", errors.New("no ELEVENLABS_API_KEY")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	w.WriteField("model_id", "scribe_v1")
	w.WriteField("tag_audio_events", "false") // words only, no "(laughs)" or "[music]"
	part, _ := w.CreateFormFile("file", filepath.Base(path))
	io.Copy(part, f)
	w.Close()
	req, _ := http.NewRequest("POST", "https://api.elevenlabs.io/v1/speech-to-text", &body)
	req.Header.Set("xi-api-key", key)
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := (&http.Client{Timeout: 2 * time.Minute}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", err
	}
	return out.Text, nil
}

func parakeet(path string) (string, error) {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, "dev", "tools", "parakeet")
	outFile := path + ".words.json"
	cmd := exec.Command(filepath.Join(dir, "Scripts", "python.exe"), filepath.Join(dir, "transcribe.py"), path, outFile)
	cmd.Env = append(os.Environ(), "HF_HUB_DISABLE_SYMLINKS_WARNING=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("parakeet: %v %s", err, strings.TrimSpace(string(out)))
	}
	defer os.Remove(outFile)
	data, err := os.ReadFile(outFile)
	if err != nil {
		return "", err
	}
	var words [][]any
	if err := json.Unmarshal(data, &words); err != nil {
		return "", err
	}
	var b strings.Builder
	for _, w := range words {
		if len(w) > 0 {
			if s, ok := w[0].(string); ok {
				b.WriteString(s + " ")
			}
		}
	}
	return b.String(), nil
}
