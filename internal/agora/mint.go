package agora

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
	"sync"
	"time"
)

// mint obtains this recorder's own join token for ch from the platform ch.Mint
// names.
func mint(ctx context.Context, client *http.Client, ch Channel) (Channel, error) {
	switch ch.Mint {
	case "superlive":
		return superlive.mint(ctx, client, ch)
	case "manyvids":
		return manyvids.mint(ctx, client, ch)
	}
	return Channel{}, fmt.Errorf("unknown token mint %q", ch.Mint)
}

// superlive mints Superlive viewer tokens. The platform hands a viewer a uid
// and token per device and broadcast, so this process registers one device
// and keeps it.
var superlive = &superliveMinter{base: "https://api.spl-live.com/api/web/"}

// superliveAppBuild is the web client release the mint presents itself as.
const superliveAppBuild = "5.23.23"

type superliveMinter struct {
	base string

	mu     sync.Mutex
	device string
}

func (m *superliveMinter) mint(ctx context.Context, client *http.Client, ch Channel) (Channel, error) {
	if ch.Room == "" {
		return Channel{}, errors.New("superlive mint needs the broadcast id")
	}
	device, err := m.deviceID(ctx, client)
	if err != nil {
		return Channel{}, err
	}
	d, reason, err := m.retrieve(ctx, client, device, ch.Room)
	if err != nil {
		m.forget(device)
		return Channel{}, err
	}
	if d.Token == "" && ch.User != "" {
		// The broadcast named may have ended and the streamer started
		// another on the same channel.
		if room, err := m.currentRoom(ctx, client, device, ch.User); err == nil && room != "" && room != ch.Room {
			d, reason, err = m.retrieve(ctx, client, device, room)
			if err != nil {
				return Channel{}, err
			}
			ch.Room = room
		}
	}
	if d.Token == "" || d.AgoraID <= 0 {
		if reason != "" {
			return Channel{}, fmt.Errorf("superlive refused the broadcast: %s", reason)
		}
		return Channel{}, errors.New("superlive returned no token")
	}
	ch.Token, ch.UID = d.Token, uint32(d.AgoraID)
	if d.ChannelID != "" {
		ch.Name = d.ChannelID
	}
	return ch, nil
}

type superliveStream struct {
	ChannelID string `json:"channel_id"`
	Token     string `json:"agora_channel_token"`
	AgoraID   int64  `json:"agora_id"`
}

// retrieve reads a broadcast's join credentials as device sees them, with the
// platform's reason when it withholds them.
func (m *superliveMinter) retrieve(ctx context.Context, client *http.Client, device, room string) (superliveStream, string, error) {
	var reply struct {
		DisallowedReason *string         `json:"disallowed_reason"`
		StreamDetails    superliveStream `json:"stream_details"`
	}
	id, err := strconv.ParseInt(room, 10, 64)
	if err != nil {
		return superliveStream{}, "", fmt.Errorf("superlive broadcast id %q: %w", room, err)
	}
	if err := m.call(ctx, client, "livestream/retrieve", device, map[string]any{"livestream_id": id}, &reply); err != nil {
		return superliveStream{}, "", err
	}
	reason := ""
	if reply.DisallowedReason != nil {
		reason = *reply.DisallowedReason
	}
	return reply.StreamDetails, reason, nil
}

// currentRoom returns the broadcast the user is live in, empty when none.
func (m *superliveMinter) currentRoom(ctx context.Context, client *http.Client, device, user string) (string, error) {
	var reply struct {
		User struct {
			LivestreamID *string `json:"livestream_id"`
		} `json:"user"`
	}
	if err := m.call(ctx, client, "users/profile", device, map[string]any{"user_id": user}, &reply); err != nil {
		return "", err
	}
	if reply.User.LivestreamID == nil {
		return "", nil
	}
	return *reply.User.LivestreamID, nil
}

func (m *superliveMinter) deviceID(ctx context.Context, client *http.Client) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.device != "" {
		return m.device, nil
	}
	var reply struct {
		GUID string `json:"guid"`
	}
	if err := m.call(ctx, client, "initialize", "", nil, &reply); err != nil {
		return "", err
	}
	if reply.GUID == "" {
		return "", errors.New("superlive registered no device")
	}
	m.device = reply.GUID
	return m.device, nil
}

// forget drops a device the platform may no longer recognise, so the next
// mint registers a fresh one.
func (m *superliveMinter) forget(device string) {
	m.mu.Lock()
	if m.device == device {
		m.device = ""
	}
	m.mu.Unlock()
}

func (m *superliveMinter) call(ctx context.Context, client *http.Client, path, device string, body map[string]any, out any) error {
	if body == nil {
		body = map[string]any{}
	}
	// A client that names no recent app build is told every broadcast has
	// finished.
	body["client_params"] = map[string]any{"os_type": "web", "app": "superlive", "app_build": superliveAppBuild,
		"web_type": 1, "app_language": "en"}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.base+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Origin", "https://superlive.chat")
	req.Header.Set("Referer", "https://superlive.chat/")
	if device != "" {
		req.Header.Set("device-id", device)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("superlive %s: %w", path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("superlive %s: %w", path, err)
	}
	var apiErr struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &apiErr) == nil && apiErr.Error != nil {
		return fmt.Errorf("superlive %s: %s (code %d)", path, apiErr.Error.Message, apiErr.Error.Code)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("superlive %s: status %d", path, resp.StatusCode)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("superlive %s: decode: %w", path, err)
	}
	return nil
}

// manyvids mints ManyVids viewer grants. Joining a room's channel as a guest
// hands out a fresh uid and token each time, so every recording takes its own.
var manyvids = &manyvidsMinter{base: "https://api.manyvids.com/live/room/"}

type manyvidsMinter struct {
	base string
}

func (m *manyvidsMinter) mint(ctx context.Context, client *http.Client, ch Channel) (Channel, error) {
	if ch.Room == "" {
		return Channel{}, errors.New("manyvids mint needs the room id")
	}
	b, _ := json.Marshal(map[string]string{"visibility": "PUBLIC"})
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.base+url.PathEscape(ch.Room)+"/joinChannel", bytes.NewReader(b))
	if err != nil {
		return Channel{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Origin", "https://www.manyvids.com")
	req.Header.Set("Referer", "https://www.manyvids.com/")
	resp, err := client.Do(req)
	if err != nil {
		return Channel{}, fmt.Errorf("manyvids joinChannel: %w", err)
	}
	defer resp.Body.Close()
	var reply struct {
		Message     string `json:"message"`
		MeetingInfo struct {
			ChannelID string `json:"channelId"`
			RTC       string `json:"rtc"`
			UID       uint32 `json:"uid"`
		} `json:"meetingInfo"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&reply); err != nil {
		return Channel{}, fmt.Errorf("manyvids joinChannel: status %d: %w", resp.StatusCode, err)
	}
	mi := reply.MeetingInfo
	if resp.StatusCode != http.StatusOK || mi.RTC == "" || mi.UID == 0 {
		// A room that is not broadcasting answers 401 "Creator is not currently Live".
		return Channel{}, fmt.Errorf("manyvids joinChannel: status %d: %s", resp.StatusCode, reply.Message)
	}
	ch.Token, ch.UID = mi.RTC, mi.UID
	if mi.ChannelID != "" {
		ch.Name = mi.ChannelID
	}
	return ch, nil
}
