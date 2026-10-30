package main

// phone mcp: the iPhone as an MCP server on stdio (JSON-RPC, one message per line), for agent
// sessions. It drives the same session as serve: the shot every coordinate refers to, the guard
// that refuses a touch on a screen that moved since the last shot, the touch lock shared with
// any other process driving the phone. The display stream and touch session close after 30 s
// without a touch, so an idle agent session does not hold the phone.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

const touchIdle = 30 * time.Second

type rpcMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// argError is a call the agent got wrong (not the phone failing): the session stays as it is.
type argError struct{ error }

type mcpServer struct {
	s *session // nil until the first call, and again after the phone went away
}

func schema(props map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": props, "required": required}
}

var num = map[string]any{"type": "number"}

var mcpTools = []map[string]any{
	{"name": "look", "description": "Screenshot of the owner's iPhone, at half size. Every x/y the other tools take is a pixel of this image. " +
		"Notes: there is no home or app-switcher gesture, switch apps with open_app and quit them with close_app; there is no keyboard typing, use type_text and paste; " +
		"the phone has to be unlocked (a passcode cannot be entered here). Sending a message, paying, deleting, or anything else " +
		"that cannot be taken back needs the user's explicit go-ahead in the conversation first.",
		"inputSchema": schema(map[string]any{})},
	{"name": "tap", "description": "Tap at (x, y) and return the screen 0.6 s later. If the screen around the point is no longer what the last " +
		"screenshot showed (a list moved, a sheet slid in), the tap is not made and the new screenshot comes back instead: aim again on it.",
		"inputSchema": schema(map[string]any{"x": num, "y": num}, "x", "y")},
	{"name": "long_press", "description": "Press and hold at (x, y) for ms (default 800), then return the screen. Guarded like tap. " +
		"Only for bringing up menus: never on a chat's hold-to-talk bar (WeChat 按住 说话), which records and sends a voice message.",
		"inputSchema": schema(map[string]any{"x": num, "y": num, "ms": num}, "x", "y")},
	{"name": "swipe", "description": "Drag from (x1, y1) to (x2, y2) over ms (default 300), then return the screen. " +
		"Scrolls lists, turns pages, moves picker wheels (tapping a number visible on a wheel also selects it).",
		"inputSchema": schema(map[string]any{"x1": num, "y1": num, "x2": num, "y2": num, "ms": num}, "x1", "y1", "x2", "y2")},
	{"name": "type_text", "description": "Put text (any language) on the iPhone's clipboard. To enter it: tap the field so the cursor and keyboard show, " +
		"then tap the clipboard suggestion above the keyboard (the one with a clipboard icon); without one, tap the cursor again and tap 粘贴 (Paste). " +
		"Check the screenshot. In WeChat, a bottom bar reading 按住 说话 is voice mode: tap the keyboard icon on its left first.",
		"inputSchema": schema(map[string]any{"text": map[string]any{"type": "string"}}, "text")},
	{"name": "open_app", "description": "Open an app fresh (quit it first, so it starts on its first screen) and return the screen. " +
		"app is a bundle id (com.apple.mobiletimer, com.apple.mobilenotes, com.tencent.xin) or the app's name as shown on the home screen.",
		"inputSchema": schema(map[string]any{"app": map[string]any{"type": "string"}}, "app")},
	{"name": "close_app", "description": "Quit an app (the same as swiping it away in the app switcher).",
		"inputSchema": schema(map[string]any{"app": map[string]any{"type": "string"}}, "app")},
	{"name": "list_apps", "description": "Installed apps (name and bundle id), filtered by query when given.",
		"inputSchema": schema(map[string]any{"query": map[string]any{"type": "string"}})},
}

func (m *mcpServer) session() (*session, error) {
	if m.s != nil {
		return m.s, nil
	}
	d, err := device()
	if err != nil {
		return nil, fmt.Errorf("no iPhone: %v (it has to be plugged into this Mac, or on its Wi-Fi, with the go-ios tunnel running)", err)
	}
	m.s = &session{d: d, refused: [2]float64{-1, -1}}
	return m.s, nil
}

func text(t string) map[string]any { return map[string]any{"type": "text", "text": t} }

func imageContent(shot map[string]any) map[string]any {
	return map[string]any{"type": "image", "data": shot["jpeg"], "mimeType": "image/jpeg"}
}

func arg(args map[string]any, key string, fallback float64) float64 {
	if v, ok := args[key].(float64); ok {
		return v
	}
	return fallback
}

// after returns the screen once an action had time to settle.
func (s *session) after(did string, settle time.Duration) ([]map[string]any, error) {
	time.Sleep(settle)
	shot, err := s.shot()
	if err != nil {
		return nil, err
	}
	return []map[string]any{imageContent(shot), text(did + " This is the screen now.")}, nil
}

func (s *session) findApp(query string) (string, string, error) {
	if err := s.loadApps(); err != nil {
		return "", "", err
	}
	var hits []app
	for _, a := range s.apps {
		if a.Bundle == query || a.Name == query {
			return a.Bundle, a.Name, nil
		}
		if strings.Contains(strings.ToLower(a.Name), strings.ToLower(query)) {
			hits = append(hits, a)
		}
	}
	if len(hits) == 1 {
		return hits[0].Bundle, hits[0].Name, nil
	}
	return "", "", argError{fmt.Errorf("no single app matches %q (%d candidates); use list_apps", query, len(hits))}
}

// call runs one tool; touched reports whether it used the touch session.
func (m *mcpServer) call(name string, args map[string]any) (content []map[string]any, touched bool, err error) {
	s, err := m.session()
	if err != nil {
		return nil, false, err
	}
	switch name {
	case "look":
		shot, err := s.shot()
		if err != nil {
			return nil, false, err
		}
		note := fmt.Sprintf("Screen %v×%v (coordinates use this size).", shot["w"], shot["h"])
		if locked, err := s.locked(); err == nil && locked {
			note += " The iPhone is locked: ask the owner to unlock it."
		}
		return []map[string]any{imageContent(shot), text(note)}, false, nil
	case "tap", "long_press":
		r := request{Cmd: "tap", X: arg(args, "x", -1), Y: arg(args, "y", -1)}
		if r.X < 0 || r.Y < 0 {
			return nil, false, argError{fmt.Errorf("x and y are required")}
		}
		if name == "long_press" {
			r.Cmd, r.Ms = "press", int(arg(args, "ms", 800))
		}
		res, err := s.handle(r)
		if err != nil {
			return nil, true, err
		}
		if res["changed"] == true {
			return []map[string]any{imageContent(res), text("Not touched: the screen around that point changed since your last screenshot. This is the new one; aim again.")}, true, nil
		}
		content, err := s.after(map[string]string{"tap": "Tapped.", "press": "Long-pressed."}[r.Cmd], 600*time.Millisecond)
		return content, true, err
	case "swipe":
		r := request{Cmd: "swipe", X1: arg(args, "x1", 0), Y1: arg(args, "y1", 0), X2: arg(args, "x2", 0), Y2: arg(args, "y2", 0), Ms: int(arg(args, "ms", 300))}
		if _, err := s.handle(r); err != nil {
			return nil, true, err
		}
		content, err := s.after("Swiped.", 600*time.Millisecond)
		return content, true, err
	case "type_text":
		t, _ := args["text"].(string)
		if _, err := s.handle(request{Cmd: "text", Text: t}); err != nil {
			return nil, false, err
		}
		return []map[string]any{text("On the clipboard. Tap the field, long-press it, tap 粘贴 (Paste).")}, false, nil
	case "open_app":
		a, _ := args["app"].(string)
		bundle, label, err := s.findApp(a)
		if err != nil {
			return nil, false, err
		}
		if _, err := s.kill(bundle); err != nil {
			return nil, false, err
		}
		time.Sleep(500 * time.Millisecond)
		if err := s.launch(bundle); err != nil {
			return nil, false, err
		}
		content, err := s.after(fmt.Sprintf("Opened %s (%s).", label, bundle), 1500*time.Millisecond)
		return content, false, err
	case "close_app":
		a, _ := args["app"].(string)
		bundle, label, err := s.findApp(a)
		if err != nil {
			return nil, false, err
		}
		killed, err := s.kill(bundle)
		if err != nil {
			return nil, false, err
		}
		if !killed {
			return []map[string]any{text(label + " was not running.")}, false, nil
		}
		return []map[string]any{text("Quit " + label + ".")}, false, nil
	case "list_apps":
		q, _ := args["query"].(string)
		if err := s.loadApps(); err != nil {
			return nil, false, err
		}
		var lines []string
		for _, a := range s.apps {
			if q == "" || strings.Contains(strings.ToLower(a.Name+" "+a.Bundle), strings.ToLower(q)) {
				lines = append(lines, a.Name+"  "+a.Bundle)
			}
		}
		if len(lines) == 0 {
			lines = []string{"no match"}
		}
		return []map[string]any{text(strings.Join(lines, "\n"))}, false, nil
	}
	return nil, false, argError{fmt.Errorf("unknown tool %q", name)}
}

func (m *mcpServer) reply(msg rpcMessage) (any, *map[string]any) {
	switch msg.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(msg.Params, &p)
		if p.ProtocolVersion == "" {
			p.ProtocolVersion = "2025-06-18"
		}
		return map[string]any{
			"protocolVersion": p.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "iphone", "version": "1"},
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": mcpTools}, nil
	}
	return nil, &map[string]any{"code": -32601, "message": "method not found: " + msg.Method}
}

func mcpServe() error {
	m := &mcpServer{}
	defer func() {
		if m.s != nil {
			m.s.close()
		}
	}()
	lines := make(chan []byte)
	go func() {
		in := bufio.NewScanner(os.Stdin)
		in.Buffer(make([]byte, 1<<20), 4<<20)
		for in.Scan() {
			lines <- append([]byte(nil), in.Bytes()...)
		}
		close(lines)
	}()
	out := json.NewEncoder(os.Stdout)
	idle := time.NewTimer(time.Hour)
	idle.Stop()
	for {
		select {
		case <-idle.C:
			if m.s != nil {
				m.s.close() // stream, touch, lock; the device and the last shot stay
			}
			continue
		case line, ok := <-lines:
			if !ok {
				return nil
			}
			var msg rpcMessage
			if err := json.Unmarshal(line, &msg); err != nil || len(msg.ID) == 0 {
				continue // notifications need no answer
			}
			resp := map[string]any{"jsonrpc": "2.0", "id": msg.ID}
			if msg.Method == "tools/call" {
				var p struct {
					Name      string         `json:"name"`
					Arguments map[string]any `json:"arguments"`
				}
				json.Unmarshal(msg.Params, &p)
				content, touched, err := m.call(p.Name, p.Arguments)
				var bad argError
				if err != nil && !errors.As(err, &bad) && err != errBusy && m.s != nil {
					// The tunnel may have been rebuilt (phone rebooted, Wi-Fi came back) on a new
					// port: look the phone up again and try once more.
					m.s.close()
					m.s = nil
					content, touched, err = m.call(p.Name, p.Arguments)
				}
				if touched {
					idle.Reset(touchIdle)
				}
				if err != nil {
					if !errors.As(err, &bad) && err != errBusy && m.s != nil {
						m.s.close() // still failing: start over on the next call
						m.s = nil
					}
					resp["result"] = map[string]any{"content": []map[string]any{text(err.Error())}, "isError": true}
				} else {
					resp["result"] = map[string]any{"content": content}
				}
			} else if result, rpcErr := m.reply(msg); rpcErr != nil {
				resp["error"] = *rpcErr
			} else {
				resp["result"] = result
			}
			if err := out.Encode(resp); err != nil {
				return err
			}
		}
	}
}
