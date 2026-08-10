package main

import (
	"flag"
	"fmt"
	"image"
	"image/png"
	"math/rand/v2"
	"os"
	"path/filepath"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"

	gocan "github.com/roffe/gocan/v2"
)

var screenshotDir = flag.String("screenshot", "", "capture a PNG of every tab into this directory, then exit")

// demoFrames are a plausible mix of SAAB T7 bus IDs: {id, period ms}.
var demoFrames = []struct {
	id     uint32
	period time.Duration
}{
	{0x1A0, 10 * time.Millisecond},
	{0x280, 20 * time.Millisecond},
	{0x290, 20 * time.Millisecond},
	{0x2F0, 50 * time.Millisecond},
	{0x370, 100 * time.Millisecond},
	{0x3A0, 100 * time.Millisecond},
	{0x5C0, 500 * time.Millisecond},
}

// fakeTraffic feeds the normal record() path so screenshots aren't empty.
// ponytail: synthetic data beats scripting a real adapter just for pictures.
func fakeTraffic(stop <-chan struct{}) {
	next := make([]time.Time, len(demoFrames))
	rng := rand.New(rand.NewPCG(1, 2)) // fixed seed: reproducible screenshots
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-tick.C:
			for i, d := range demoFrames {
				if now.Before(next[i]) {
					continue
				}
				next[i] = now.Add(d.period)
				data := make([]byte, 8)
				for j := range data {
					data[j] = byte(rng.UintN(256))
				}
				record(gocan.NewFrame(d.id, data))
			}
		}
	}
}

// capture screenshots every tab of w and quits the app.
func captureTabs(a fyne.App, w fyne.Window, tabs *container.AppTabs, dir string) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "screenshot:", err)
		a.Quit()
		return
	}

	stop := make(chan struct{})
	go fakeTraffic(stop)

	txMu.Lock()
	txMsgs = append(txMsgs,
		&txMsg{id: 0x220, data: []byte{0x3F, 0x81, 0x00, 0x11, 0x02, 0x40, 0x00, 0x00}, period: 100 * time.Millisecond, running: true, count: 42},
		&txMsg{id: 0x240, data: []byte{0x40, 0xA1, 0x02, 0x1A, 0x90}, period: 0},
	)
	txMu.Unlock()

	time.Sleep(2 * time.Second) // let the window map, traffic accumulate, UI tick

	for i, tab := range tabs.Items {
		fyne.DoAndWait(func() { tabs.SelectIndex(i) })
		time.Sleep(500 * time.Millisecond) // tab switch + the 200ms refresh tick
		name := filepath.Join(dir, fmt.Sprintf("%02d-%s.png", i+1, tab.Text))
		if err := writePNG(name, w); err != nil {
			fmt.Fprintln(os.Stderr, "screenshot:", err)
			continue
		}
		fmt.Println("wrote", name)
	}

	close(stop)
	a.Quit()
}

func writePNG(name string, w fyne.Window) error {
	var img image.Image
	fyne.DoAndWait(func() { img = w.Canvas().Capture() })
	f, err := os.Create(name)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		return err
	}
	return f.Close()
}
