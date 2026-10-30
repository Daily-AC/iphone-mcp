package main

// phone serve: one session for one task, driven by JSON lines on stdin, one JSON reply per line
// on stdout. The display stream (touch needs it) and the touch session open on the first touch
// and stay up until stdin closes, so a task of many steps pays for them once.
//
// Coordinates are those of the shot the model sees: the screen at half size (603x1311 on an
// iPhone18,3), converted back to screen pixels here.
//
//	{"cmd":"shot"}                                -> {"ok":true,"jpeg":"<base64>","w":603,"h":1311}
//	{"cmd":"tap","x":300,"y":650}                 -> {"ok":true} or, when the screen around the point
//	                                                 changed since the last shot, no tap and
//	                                                 {"ok":true,"changed":true,"jpeg":…} (the new shot)
//	{"cmd":"press","x":300,"y":650,"ms":800}      long press, guarded the same way
//	{"cmd":"swipe","x1":300,"y1":1000,"x2":300,"y2":400,"ms":300}
//	{"cmd":"text","text":"你好"}                   puts it on the clipboard (then long-press a field, tap 粘贴)
//	{"cmd":"launch","bundle":"com.apple.mobiletimer"}
//	{"cmd":"kill","bundle":"com.apple.mobiletimer"}
//	{"cmd":"apps"}                                -> {"ok":true,"apps":[{"bundle":"…","name":"…"}]}
//	{"cmd":"locked"}                              -> {"ok":true,"locked":false}
//	{"cmd":"wait","ms":500}

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/danielpaulus/go-ios/ios"
	"github.com/danielpaulus/go-ios/ios/hid"
	"github.com/danielpaulus/go-ios/ios/installationproxy"
	"github.com/danielpaulus/go-ios/ios/instruments"
	"github.com/danielpaulus/go-ios/ios/pasteboard"
)

const shotScale = 2 // the model sees the screen at 1/shotScale

type request struct {
	Cmd    string  `json:"cmd"`
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	X1     float64 `json:"x1"`
	Y1     float64 `json:"y1"`
	X2     float64 `json:"x2"`
	Y2     float64 `json:"y2"`
	Ms     int     `json:"ms"`
	Text   string  `json:"text"`
	Bundle string  `json:"bundle"`
}

type app struct {
	Bundle string `json:"bundle"`
	Name   string `json:"name"`
	exe    string
}

type session struct {
	d          ios.DeviceEntry
	stopStream func()
	touch      *hid.Session
	apps       []app
	last       *image.RGBA // the last shot handed out: what the model believes is on screen
	refused    [2]float64  // the point of a touch just refused for a changed screen
	lock       *os.File    // held while touching: one driver of the iPhone at a time
}

func (s *session) close() {
	if s.touch != nil {
		s.touch.Close()
		s.touch = nil
	}
	if s.stopStream != nil {
		s.stopStream()
		s.stopStream = nil
	}
	if s.lock != nil {
		s.lock.Close() // releases the flock
		s.lock = nil
	}
}

// errBusy: another process (another agent session, a script) is touching the iPhone.
var errBusy = fmt.Errorf("the iPhone is being driven by another task right now; try again in a minute")

// lockPath is the file every process driving the iPhone flocks while touching: IPHONE_MCP_LOCK,
// or ~/.local/share/iphone-mcp/touch.lock.
func lockPath() string {
	if p := os.Getenv("IPHONE_MCP_LOCK"); p != "" {
		return p
	}
	return filepath.Join(os.Getenv("HOME"), ".local/share/iphone-mcp/touch.lock")
}

func (s *session) hid() (*hid.Session, error) {
	if s.touch != nil {
		return s.touch, nil
	}
	path := lockPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, errBusy
	}
	stop, err := startStream(s.d)
	if err != nil {
		lock.Close()
		return nil, err
	}
	t, err := hid.NewSession(s.d)
	if err != nil {
		stop()
		lock.Close()
		return nil, err
	}
	s.stopStream, s.touch, s.lock = stop, t, lock
	time.Sleep(300 * time.Millisecond) // the first touch right after the stream starts is lost
	return t, nil
}

// at converts shot coordinates to a touch point.
func at(x, y float64) hid.Point { return point(x*shotScale, y*shotScale) }

func (s *session) press(x, y float64, hold time.Duration) error {
	t, err := s.hid()
	if err != nil {
		return err
	}
	if err := t.TouchDown(at(x, y)); err != nil {
		return err
	}
	time.Sleep(hold)
	return t.TouchUp(at(x, y))
}

func (s *session) swipe(r request) error {
	t, err := s.hid()
	if err != nil {
		return err
	}
	ms := r.Ms
	if ms <= 0 {
		ms = 300
	}
	steps := ms / 8
	if steps < 2 {
		steps = 2
	}
	for i := 0; i <= steps; i++ {
		f := float64(i) / float64(steps)
		if err := t.TouchDown(at(r.X1+(r.X2-r.X1)*f, r.Y1+(r.Y2-r.Y1)*f)); err != nil {
			return err
		}
		time.Sleep(8 * time.Millisecond)
	}
	return t.TouchUp(at(r.X2, r.Y2))
}

func (s *session) shot() (map[string]any, error) {
	svc, err := instruments.NewScreenshotService(s.d)
	if err != nil {
		return nil, err
	}
	defer svc.Close()
	raw, err := svc.TakeScreenshot()
	if err != nil {
		return nil, err
	}
	src, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	small := shrink(src, shotScale)
	s.last = small
	var out bytes.Buffer
	if err := jpeg.Encode(&out, small, &jpeg.Options{Quality: 75}); err != nil {
		return nil, err
	}
	b := small.Bounds()
	return map[string]any{"jpeg": base64.StdEncoding.EncodeToString(out.Bytes()), "w": b.Dx(), "h": b.Dy()}, nil
}

// shrink averages k x k blocks (k = 2: text stays legible, the shot is a quarter of the pixels).
func shrink(src image.Image, k int) *image.RGBA {
	sb := src.Bounds()
	w, h := sb.Dx()/k, sb.Dy()/k
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	n := uint32(k * k)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var r, g, b uint32
			for dy := 0; dy < k; dy++ {
				for dx := 0; dx < k; dx++ {
					cr, cg, cb, _ := src.At(sb.Min.X+x*k+dx, sb.Min.Y+y*k+dy).RGBA()
					r, g, b = r+cr, g+cg, b+cb
				}
			}
			dst.SetRGBA(x, y, color.RGBA{uint8(r / n >> 8), uint8(g / n >> 8), uint8(b / n >> 8), 255})
		}
	}
	return dst
}

func (s *session) loadApps() error {
	if s.apps != nil {
		return nil
	}
	conn, err := installationproxy.New(s.d)
	if err != nil {
		return err
	}
	defer conn.Close()
	all, err := conn.BrowseAllApps()
	if err != nil {
		return err
	}
	for _, a := range all {
		name, _ := a[installationproxy.CFBundleDisplayName].(string)
		if name == "" {
			name = a.CFBundleName()
		}
		s.apps = append(s.apps, app{Bundle: a.CFBundleIdentifier(), Name: name, exe: a.CFBundleExecutable()})
	}
	return nil
}

func (s *session) launch(bundle string) error {
	pc, err := instruments.NewProcessControl(s.d)
	if err != nil {
		return err
	}
	defer pc.Close()
	_, err = pc.LaunchApp(bundle, nil)
	return err
}

func (s *session) kill(bundle string) (bool, error) {
	if err := s.loadApps(); err != nil {
		return false, err
	}
	exe := ""
	for _, a := range s.apps {
		if a.Bundle == bundle {
			exe = a.exe
		}
	}
	if exe == "" {
		return false, fmt.Errorf("no app %s", bundle)
	}
	info, err := instruments.NewDeviceInfoService(s.d)
	if err != nil {
		return false, err
	}
	defer info.Close()
	procs, err := info.ProcessList()
	if err != nil {
		return false, err
	}
	pc, err := instruments.NewProcessControl(s.d)
	if err != nil {
		return false, err
	}
	defer pc.Close()
	killed := false
	for _, p := range procs {
		if p.Name == exe {
			if err := pc.KillProcess(p.Pid); err != nil {
				return killed, err
			}
			killed = true
		}
	}
	return killed, nil
}

func (s *session) locked() (bool, error) {
	conn, err := ios.ConnectLockdownWithSession(s.d)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	v, err := conn.GetValue("PasswordProtected")
	if err != nil {
		return false, err
	}
	b, _ := v.(bool)
	return b, nil
}

// moved reports, with a fresh shot, whether the screen around (x, y) is no longer what the last
// shot showed: a list that shifted under a loading banner, a page still sliding in. Touching
// there would hit something the model never saw. A second try at the same point goes through,
// so a region that never holds still (a video) cannot block a touch for good.
func (s *session) moved(x, y float64) (map[string]any, error) {
	before := s.last
	p := [2]float64{x, y}
	if before == nil || s.refused == p {
		s.refused = [2]float64{-1, -1}
		return nil, nil
	}
	now, err := s.shot()
	if err != nil {
		return nil, err
	}
	if diff(before, s.last, int(x), int(y), 100, 50) < 10 {
		return nil, nil
	}
	s.refused = p
	now["changed"] = true
	return now, nil
}

// diff is the mean absolute luminance difference of two shots in the box (cx±rx, cy±ry).
func diff(a, b *image.RGBA, cx, cy, rx, ry int) float64 {
	r := image.Rect(cx-rx, cy-ry, cx+rx, cy+ry).Intersect(a.Bounds()).Intersect(b.Bounds())
	if r.Empty() {
		return 0
	}
	lum := func(c color.RGBA) int { return (299*int(c.R) + 587*int(c.G) + 114*int(c.B)) / 1000 }
	sum := 0
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			d := lum(a.RGBAAt(x, y)) - lum(b.RGBAAt(x, y))
			if d < 0 {
				d = -d
			}
			sum += d
		}
	}
	return float64(sum) / float64(r.Dx()*r.Dy())
}

func (s *session) handle(r request) (map[string]any, error) {
	switch r.Cmd {
	case "shot":
		return s.shot()
	case "tap", "press":
		if changed, err := s.moved(r.X, r.Y); changed != nil || err != nil {
			return changed, err
		}
		if r.Cmd == "tap" {
			return nil, s.press(r.X, r.Y, 60*time.Millisecond)
		}
		ms := r.Ms
		if ms <= 0 {
			ms = 800
		}
		return nil, s.press(r.X, r.Y, time.Duration(ms)*time.Millisecond)
	case "swipe":
		return nil, s.swipe(r)
	case "text":
		pb, err := pasteboard.New(s.d)
		if err != nil {
			return nil, err
		}
		defer pb.Close()
		return nil, pb.SetText(r.Text)
	case "launch":
		return nil, s.launch(r.Bundle)
	case "kill":
		killed, err := s.kill(r.Bundle)
		return map[string]any{"killed": killed}, err
	case "apps":
		if err := s.loadApps(); err != nil {
			return nil, err
		}
		return map[string]any{"apps": s.apps}, nil
	case "locked":
		l, err := s.locked()
		return map[string]any{"locked": l}, err
	case "wait":
		time.Sleep(time.Duration(r.Ms) * time.Millisecond)
		return nil, nil
	}
	return nil, fmt.Errorf("unknown cmd %q", r.Cmd)
}

func serve() error {
	d, err := device()
	if err != nil {
		return err
	}
	s := &session{d: d, refused: [2]float64{-1, -1}}
	defer s.close()
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<20)
	out := json.NewEncoder(os.Stdout)
	for in.Scan() {
		var r request
		reply := map[string]any{}
		start := time.Now()
		if err := json.Unmarshal(in.Bytes(), &r); err != nil {
			reply["ok"], reply["error"] = false, err.Error()
		} else if res, err := s.handle(r); err != nil {
			reply["ok"], reply["error"] = false, err.Error()
		} else {
			for k, v := range res {
				reply[k] = v
			}
			reply["ok"] = true
		}
		reply["ms"] = time.Since(start).Milliseconds()
		if err := out.Encode(reply); err != nil {
			return err
		}
	}
	return in.Err()
}
