// phone: see and touch an iPhone over CoreDevice developer services (iOS 27+).
//
//	phone tap X Y [X Y ...]   tap screen pixels (screenshot coordinates), one after another
//	phone shot FILE           screenshot (PNG)
//	phone do STEP...          one stream for a sequence: tap X Y | swipe X1 Y1 X2 Y2 STEPS | wait MS | shot FILE
//	phone wifi on|off|status  lockdown over Wi-Fi (Finder's "Show this iPhone when on Wi-Fi"); needs USB
//	phone serve               a session for one task, JSON lines on stdin (see serve.go)
//	phone mcp                 the same as an MCP server on stdio, for agent sessions (see mcp.go)
//
// Needs `go-ios tunnel start` running and the developer disk image mounted. Touch only lands
// while a display stream runs, so tap starts one first (receiver on the host's tunnel address;
// with the userspace tunnel nothing reads the RTP, the stream just has to exist).
package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/danielpaulus/go-ios/ios"
	"github.com/danielpaulus/go-ios/ios/display"
	"github.com/danielpaulus/go-ios/ios/hid"
	"github.com/danielpaulus/go-ios/ios/instruments"
	"github.com/danielpaulus/go-ios/ios/tunnel"
)

// iPhone18,3 screenshots are 1206x2622 px; touch points are 0..65535 on each axis.
const screenW, screenH = 1206, 2622

func device() (ios.DeviceEntry, error) {
	d, err := ios.GetDevice(os.Getenv("GO_IOS_UDID"))
	if err != nil {
		return d, err
	}
	info, err := tunnel.TunnelInfoForDevice(d.Properties.SerialNumber, ios.HttpApiHost(), ios.HttpApiPort())
	if err != nil {
		return d, fmt.Errorf("no tunnel (run go-ios tunnel start): %w", err)
	}
	d.UserspaceTUN, d.UserspaceTUNHost, d.UserspaceTUNPort = info.UserspaceTUN, ios.HttpApiHost(), info.UserspaceTUNPort
	rsd, err := ios.NewWithAddrPortDevice(info.Address, info.RsdPort, d)
	if err != nil {
		return d, err
	}
	defer rsd.Close()
	provider, err := rsd.Handshake()
	if err != nil {
		return d, err
	}
	d2, err := ios.GetDeviceWithAddress(d.Properties.SerialNumber, info.Address, provider)
	d2.UserspaceTUN, d2.UserspaceTUNHost, d2.UserspaceTUNPort = info.UserspaceTUN, ios.HttpApiHost(), info.UserspaceTUNPort
	return d2, err
}

// startStream starts the display stream touch needs and returns a stop function.
func startStream(d ios.DeviceEntry) (func(), error) {
	recvIP, recvPort := "", 0
	var recv *display.Receiver
	if d.UserspaceTUN {
		// The userspace stack has no UDP: point the stream at our side of the tunnel and let it drop.
		ip := net.ParseIP(d.Address)
		if ip == nil {
			return nil, fmt.Errorf("bad tunnel address %q", d.Address)
		}
		ip[15] ^= 3 // ...::1 (device) -> ...::2 (host)
		recvIP, recvPort = ip.String(), 50000
	} else {
		r, err := display.OpenReceiver(d)
		if err != nil {
			return nil, err
		}
		recv, recvIP, recvPort = r, r.IP(), r.Port()
		go func() {
			buf := make([]byte, 65536)
			for {
				if _, err := r.Read(buf); err != nil {
					return
				}
			}
		}()
	}
	svc, err := display.New(d)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	id, err := svc.StartVideoStream(ctx, display.VideoStreamRequest{ReceiverIP: recvIP, ReceiverPort: recvPort, SenderIP: d.Address})
	if err != nil {
		svc.Close()
		return nil, fmt.Errorf("start stream: %w", err)
	}
	return func() {
		stop, err := display.New(d)
		if err == nil {
			c, cc := context.WithTimeout(context.Background(), 5*time.Second)
			stop.StopMediaStream(c, id)
			cc()
			stop.Close()
		}
		svc.Close()
		if recv != nil {
			recv.Close()
		}
	}, nil
}

func point(x, y float64) hid.Point {
	return hid.Point{X: uint16(x / screenW * 65535), Y: uint16(y / screenH * 65535)}
}

func tap(args []string) error {
	if len(args) == 0 || len(args)%2 != 0 {
		return fmt.Errorf("tap X Y [X Y ...]")
	}
	d, err := device()
	if err != nil {
		return err
	}
	t0 := time.Now()
	stop, err := startStream(d)
	if err != nil {
		return err
	}
	defer stop()
	fmt.Printf("stream up in %d ms\n", time.Since(t0).Milliseconds())
	s, err := hid.NewSession(d)
	if err != nil {
		return err
	}
	defer s.Close()
	time.Sleep(300 * time.Millisecond)
	for i := 0; i < len(args); i += 2 {
		x, _ := strconv.ParseFloat(args[i], 64)
		y, _ := strconv.ParseFloat(args[i+1], 64)
		p := point(x, y)
		if err := s.TouchDown(p); err != nil {
			return err
		}
		time.Sleep(60 * time.Millisecond)
		if err := s.TouchUp(p); err != nil {
			return err
		}
		fmt.Printf("tapped %s,%s\n", args[i], args[i+1])
		time.Sleep(700 * time.Millisecond)
	}
	return nil
}

// swipe drags one finger from (x1,y1) to (x2,y2) in ~steps*16 ms.
func swipe(s *hid.Session, x1, y1, x2, y2 float64, steps int) error {
	for i := 0; i <= steps; i++ {
		f := float64(i) / float64(steps)
		if err := s.TouchDown(point(x1+(x2-x1)*f, y1+(y2-y1)*f)); err != nil {
			return err
		}
		time.Sleep(8 * time.Millisecond)
	}
	return s.TouchUp(point(x2, y2))
}

func do(steps []string) error {
	d, err := device()
	if err != nil {
		return err
	}
	stop, err := startStream(d)
	if err != nil {
		return err
	}
	defer stop()
	s, err := hid.NewSession(d)
	if err != nil {
		return err
	}
	defer s.Close()
	time.Sleep(300 * time.Millisecond)
	for i := 0; i < len(steps); i++ {
		switch steps[i] {
		case "swipe": // swipe X1 Y1 X2 Y2 STEPS (8 ms a step)
			var v [5]float64
			for k := range v {
				v[k], _ = strconv.ParseFloat(steps[i+1+k], 64)
			}
			i += 5
			if err := swipe(s, v[0], v[1], v[2], v[3], int(v[4])); err != nil {
				return err
			}
		case "tap":
			x, _ := strconv.ParseFloat(steps[i+1], 64)
			y, _ := strconv.ParseFloat(steps[i+2], 64)
			i += 2
			if err := s.TouchDown(point(x, y)); err != nil {
				return err
			}
			time.Sleep(60 * time.Millisecond)
			if err := s.TouchUp(point(x, y)); err != nil {
				return err
			}
		case "wait":
			ms, _ := strconv.Atoi(steps[i+1])
			i++
			time.Sleep(time.Duration(ms) * time.Millisecond)
		case "shot":
			if err := shotOn(d, steps[i+1]); err != nil {
				return err
			}
			i++
		default:
			return fmt.Errorf("unknown step %q", steps[i])
		}
	}
	return nil
}

func shot(file string) error {
	d, err := device()
	if err != nil {
		return err
	}
	return shotOn(d, file)
}

func shotOn(d ios.DeviceEntry, file string) error {
	svc, err := instruments.NewScreenshotService(d)
	if err != nil {
		return err
	}
	defer svc.Close()
	png, err := svc.TakeScreenshot()
	if err != nil {
		return err
	}
	return os.WriteFile(file, png, 0o600)
}

const wirelessDomain = "com.apple.mobile.wireless_lockdown"

func wifi(arg string) error {
	d, err := ios.GetDevice(os.Getenv("GO_IOS_UDID"))
	if err != nil {
		return err
	}
	conn, err := ios.ConnectLockdownWithSession(d)
	if err != nil {
		return err
	}
	defer conn.Close()
	switch arg {
	case "on", "off":
		if err := conn.SetValueForDomain("EnableWifiConnections", wirelessDomain, arg == "on"); err != nil {
			return err
		}
	case "status":
	default:
		return fmt.Errorf("wifi on|off|status")
	}
	v, err := conn.GetValueForDomain("EnableWifiConnections", wirelessDomain)
	if err != nil {
		return err
	}
	fmt.Printf("EnableWifiConnections: %v\n", v)
	return nil
}

func main() {
	var err error
	switch {
	case len(os.Args) > 1 && os.Args[1] == "tap":
		err = tap(os.Args[2:])
	case len(os.Args) > 1 && os.Args[1] == "do":
		err = do(os.Args[2:])
	case len(os.Args) > 2 && os.Args[1] == "shot":
		err = shot(os.Args[2])
	case len(os.Args) > 2 && os.Args[1] == "wifi":
		err = wifi(os.Args[2])
	case len(os.Args) > 1 && os.Args[1] == "mcp":
		err = mcpServe()
	case len(os.Args) > 1 && os.Args[1] == "serve":
		err = serve()
	default:
		err = fmt.Errorf("usage: phone mcp | phone serve | phone shot FILE | phone tap X Y [X Y ...] | phone do tap X Y|swipe X1 Y1 X2 Y2 STEPS|wait MS|shot FILE ... | phone wifi on|off|status")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "phone:", strings.TrimSpace(err.Error()))
		os.Exit(1)
	}
}
