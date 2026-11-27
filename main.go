package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"image/color"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	gocan "github.com/roffe/gocan/v2"
	_ "github.com/roffe/gocan/v2/adapters/all"
	_ "github.com/roffe/gocan/v2/adapters/combi" // cgo, not in all
)

type idStat struct {
	count  uint64
	last   gocan.Frame
	lastAt time.Time
	period time.Duration // EWMA of inter-frame interval
}

type row struct {
	id     uint32
	count  uint64
	period time.Duration
	frame  gocan.Frame
}

type traceEntry struct {
	at time.Time
	f  gocan.Frame
}

const maxTrace = 50000

var (
	statsMu sync.Mutex
	stats   = map[uint32]*idStat{}
	trace   []traceEntry
	logFile *os.File // non-nil while recording; guarded by statsMu
)

func record(f gocan.Frame) {
	if !filterPtr.Load().pass(f.ID) {
		return
	}
	now := time.Now()
	statsMu.Lock()
	if len(trace) >= maxTrace {
		trace = append(trace[:0], trace[maxTrace/2:]...) // drop oldest half
	}
	trace = append(trace, traceEntry{at: now, f: f})
	if logFile != nil {
		idw := 3
		if f.Extended {
			idw = 8
		}
		// candump-compatible: (epoch.micros) can0 ID#HEXDATA
		fmt.Fprintf(logFile, "(%d.%06d) can0 %0*X#%X\n", now.Unix(), now.Nanosecond()/1000, idw, f.ID, f.Data[:f.Length])
	}
	s, ok := stats[f.ID]
	if !ok {
		s = &idStat{}
		stats[f.ID] = s
	}
	s.count++
	if !s.lastAt.IsZero() {
		d := now.Sub(s.lastAt)
		if s.period == 0 {
			s.period = d
		} else {
			s.period = (s.period*4 + d) / 5
		}
	}
	s.lastAt = now
	s.last = f
	statsMu.Unlock()
}

var (
	logMu    sync.Mutex
	logLines []string
)

func logf(format string, args ...any) {
	line := time.Now().Format("15:04:05.000") + "  " + fmt.Sprintf(format, args...)
	logMu.Lock()
	if len(logLines) >= 5000 {
		logLines = append(logLines[:0], logLines[2500:]...) // drop oldest half
	}
	logLines = append(logLines, line)
	logMu.Unlock()
}

func snapshot() ([]row, []traceEntry) {
	statsMu.Lock()
	defer statsMu.Unlock()
	rows := make([]row, 0, len(stats))
	for id, s := range stats {
		rows = append(rows, row{id: id, count: s.count, period: s.period, frame: s.last})
	}
	slices.SortFunc(rows, func(a, b row) int { return int(a.id) - int(b.id) })
	return rows, slices.Clone(trace)
}

func ascii(data []byte) string {
	b := make([]byte, len(data))
	for i, c := range data {
		if c < 0x20 || c > 0x7e {
			c = '.'
		}
		b[i] = c
	}
	return string(b)
}

type compactTheme struct{ fyne.Theme }

func (t compactTheme) Size(n fyne.ThemeSizeName) float32 {
	switch n {
	case theme.SizeNameInnerPadding:
		return 2
	case theme.SizeNamePadding:
		return 1
	case theme.SizeNameText:
		return 12
	default:
		return t.Theme.Size(n)
	}
}

func minWidth(o fyne.CanvasObject, w float32) fyne.CanvasObject {
	r := canvas.NewRectangle(color.Transparent)
	r.SetMinSize(fyne.NewSize(w, 0))
	return container.NewStack(r, o)
}

func minHeight(o fyne.CanvasObject, h float32) fyne.CanvasObject {
	r := canvas.NewRectangle(color.Transparent)
	r.SetMinSize(fyne.NewSize(0, h))
	return container.NewStack(r, o)
}

type txMsg struct {
	id       uint32
	extended bool
	data     []byte
	period   time.Duration // 0 = single shot only
	running  bool
	count    uint64
	lastAt   time.Time
}

func (m *txMsg) frame() gocan.Frame {
	if m.extended {
		return gocan.NewExtendedFrame(m.id, m.data)
	}
	return gocan.NewFrame(m.id, m.data)
}

func (m *txMsg) String() string {
	period := "off"
	if m.period > 0 {
		period = strconv.FormatInt(m.period.Milliseconds(), 10) + "ms"
	}
	idw := 3
	if m.extended {
		idw = 8
	}
	return fmt.Sprintf("%0*X  [%d]  %-23s  %7s  %8d", idw, m.id, len(m.data), fmt.Sprintf("% X", m.data), period, m.count)
}

var (
	txMu   sync.Mutex
	txMsgs []*txMsg
	txBus  atomic.Pointer[gocan.Bus]
)

func parseTx(idStr, dataStr, periodStr string, extended bool) (*txMsg, error) {
	id, err := strconv.ParseUint(strings.TrimPrefix(strings.TrimSpace(idStr), "0x"), 16, 32)
	if err != nil {
		return nil, fmt.Errorf("bad ID %q", idStr)
	}
	data, err := hex.DecodeString(strings.ReplaceAll(strings.TrimSpace(dataStr), " ", ""))
	if err != nil {
		return nil, fmt.Errorf("bad data %q", dataStr)
	}
	if len(data) > 8 {
		return nil, fmt.Errorf("data too long: %d bytes", len(data))
	}
	ms := 0
	if s := strings.TrimSpace(periodStr); s != "" {
		if ms, err = strconv.Atoi(s); err != nil || ms < 0 {
			return nil, fmt.Errorf("bad period %q", periodStr)
		}
	}
	return &txMsg{id: uint32(id), extended: extended, data: data, period: time.Duration(ms) * time.Millisecond}, nil
}

func txSend(f gocan.Frame) error {
	b := txBus.Load()
	if b == nil {
		return fmt.Errorf("not connected")
	}
	ctx, cancel := context.WithTimeout(b.Context(), time.Second)
	defer cancel()
	return b.Send(ctx, f)
}

// txScheduler sends every running message when its period is due.
// ponytail: 1ms scan for all messages, good enough for ms-resolution periods.
func txScheduler() {
	for now := range time.Tick(time.Millisecond) {
		if txBus.Load() == nil {
			continue
		}
		var due []gocan.Frame
		txMu.Lock()
		for _, m := range txMsgs {
			if m.running && m.period > 0 && now.Sub(m.lastAt) >= m.period {
				m.lastAt = now
				m.count++
				due = append(due, m.frame())
			}
		}
		txMu.Unlock()
		for _, f := range due {
			if err := txSend(f); err != nil {
				logf("tx 0x%X: %v", f.ID, err)
			}
		}
	}
}

func main() {
	log.SetFlags(log.Lshortfile | log.LstdFlags)
	flag.Parse()

	a := app.NewWithID("com.roffe.canmonitor") // explicit ID so Preferences work in plain `go build` too
	w := a.NewWindow("CAN Monitor")

	adapterNames := gocan.AdapterNames()
	adapterSel := widget.NewSelect(adapterNames, nil)
	var adapterW float32
	for _, n := range adapterNames {
		if s := fyne.MeasureText(n, theme.TextSize(), fyne.TextStyle{}); s.Width > adapterW {
			adapterW = s.Width
		}
	}
	adapterW += theme.IconInlineSize() + theme.InnerPadding()*4
	portEntry := newPortEntry()
	refreshBtn := widget.NewButtonWithIcon("", theme.ViewRefreshIcon(), func() {
		gocan.Rescan() // picks up hotplugged devices
		adapterSel.SetOptions(gocan.AdapterNames())
	})
	rateEntry := widget.NewSelectEntry([]string{"33.3", "47.619", "500", "615.384", "1000"})
	restoreSettings(a.Preferences(), adapterSel, portEntry, rateEntry)
	status := widget.NewLabel("Disconnected")

	var rows []row // only touched on the Fyne thread

	table := widget.NewTableWithHeaders(
		func() (int, int) { return len(rows), 5 },
		func() fyne.CanvasObject {
			l := widget.NewLabel("")
			l.TextStyle.Monospace = true
			return l
		},
		func(id widget.TableCellID, o fyne.CanvasObject) {
			r := rows[id.Row]
			l := o.(*widget.Label)
			switch id.Col {
			case 0:
				l.SetText(fmt.Sprintf("0x%03X", r.id))
			case 1:
				l.SetText(strconv.FormatUint(r.count, 10))
			case 2:
				if r.period > 0 {
					l.SetText(fmt.Sprintf("%.1fms", float64(r.period.Microseconds())/1000))
				} else {
					l.SetText("-")
				}
			case 3:
				l.SetText(fmt.Sprintf("% X", r.frame.Data[:r.frame.Length]))
			case 4:
				l.SetText(ascii(r.frame.Data[:r.frame.Length]))
			}
		},
	)
	table.ShowHeaderColumn = false
	headers := []string{"ID", "Count", "Period", "Data", "ASCII"}
	table.CreateHeader = func() fyne.CanvasObject { return widget.NewLabel("") }
	table.UpdateHeader = func(id widget.TableCellID, o fyne.CanvasObject) {
		o.(*widget.Label).SetText(headers[id.Col])
	}
	for i, width := range []float32{80, 80, 90, 230, 110} {
		table.SetColumnWidth(i, width)
	}

	var traceRows []traceEntry // only touched on the Fyne thread

	traceList := widget.NewList(
		func() int { return len(traceRows) },
		func() fyne.CanvasObject {
			l := widget.NewLabel("")
			l.TextStyle.Monospace = true
			return l
		},
		func(i widget.ListItemID, o fyne.CanvasObject) {
			e := traceRows[i]
			data := e.f.Data[:e.f.Length]
			o.(*widget.Label).SetText(fmt.Sprintf("%s  0x%03X  %-23s  |%s|",
				e.at.Format("15:04:05.000"), e.f.ID, fmt.Sprintf("% X", data), ascii(data)))
		},
	)
	traceList.HideSeparators = true

	var logRows []string // only touched on the Fyne thread
	logList := widget.NewList(
		func() int { return len(logRows) },
		func() fyne.CanvasObject {
			l := widget.NewLabel("")
			l.TextStyle.Monospace = true
			return l
		},
		func(i widget.ListItemID, o fyne.CanvasObject) {
			o.(*widget.Label).SetText(logRows[i])
		},
	)
	logList.HideSeparators = true
	autoscroll := widget.NewCheck("Autoscroll", nil)
	autoscroll.SetChecked(true)

	recordBtn := widget.NewButton("Record", nil)
	recordBtn.OnTapped = func() {
		statsMu.Lock()
		defer statsMu.Unlock()
		if logFile != nil {
			logFile.Close()
			logf("recording stopped: %s", logFile.Name())
			logFile = nil
			recordBtn.SetText("Record")
			return
		}
		name := "CAN_" + time.Now().Format("2006-01-02_15-04-05") + ".log"
		if home, err := os.UserHomeDir(); err == nil {
			name = filepath.Join(home, name)
		}
		f, err := os.Create(name)
		if err != nil {
			logf("recording: %v", err)
			status.SetText("Error: " + err.Error())
			return
		}
		logFile = f
		logf("recording to %s", name)
		recordBtn.SetText("Stop: " + name)
	}

	txList := widget.NewList(
		func() int { txMu.Lock(); defer txMu.Unlock(); return len(txMsgs) },
		func() fyne.CanvasObject {
			l := widget.NewLabel("")
			l.TextStyle.Monospace = true
			return container.NewHBox(widget.NewCheck("", nil), l)
		},
		func(i widget.ListItemID, o fyne.CanvasObject) {
			txMu.Lock()
			defer txMu.Unlock()
			if i >= len(txMsgs) {
				return
			}
			m := txMsgs[i]
			row := o.(*fyne.Container)
			check := row.Objects[0].(*widget.Check)
			check.OnChanged = nil // SetChecked would fire it while we hold txMu
			check.SetChecked(m.running)
			check.OnChanged = func(on bool) {
				txMu.Lock()
				m.running = on && m.period > 0
				m.lastAt = time.Time{}
				txMu.Unlock()
			}
			row.Objects[1].(*widget.Label).SetText(m.String())
		},
	)
	txList.HideSeparators = true

	txID := widget.NewEntry()
	txID.SetText("000")
	txData := widget.NewEntry()
	txData.SetText("00 00 00 00 00 00 00 00")
	txPeriod := widget.NewEntry()
	txPeriod.SetText("0")
	txExt := widget.NewCheck("29 bit", nil)
	selected := -1
	txList.OnSelected = func(i widget.ListItemID) {
		txMu.Lock()
		defer txMu.Unlock()
		if i >= len(txMsgs) {
			return
		}
		selected = i
		m := txMsgs[i]
		txID.SetText(fmt.Sprintf("%X", m.id))
		txData.SetText(fmt.Sprintf("% X", m.data))
		txPeriod.SetText(strconv.FormatInt(m.period.Milliseconds(), 10))
		txExt.SetChecked(m.extended)
	}
	txList.OnUnselected = func(widget.ListItemID) { selected = -1 }

	txEdit := func() (*txMsg, bool) {
		m, err := parseTx(txID.Text, txData.Text, txPeriod.Text, txExt.Checked)
		if err != nil {
			logf("tx: %v", err)
			status.SetText("Error: " + err.Error())
			return nil, false
		}
		return m, true
	}
	txSingle := widget.NewButton("Single Shot", func() {
		m, ok := txEdit()
		if !ok {
			return
		}
		if err := txSend(m.frame()); err != nil {
			logf("tx 0x%X: %v", m.id, err)
			status.SetText("Error: " + err.Error())
		}
	})
	txAdd := widget.NewButton("Add", func() {
		m, ok := txEdit()
		if !ok {
			return
		}
		txMu.Lock()
		txMsgs = append(txMsgs, m)
		txMu.Unlock()
		txList.Refresh()
	})
	txDelete := widget.NewButton("Delete", func() {
		txMu.Lock()
		if selected >= 0 && selected < len(txMsgs) {
			txMsgs = slices.Delete(txMsgs, selected, selected+1)
		}
		txMu.Unlock()
		txList.UnselectAll()
		txList.Refresh()
	})
	txAll := func(on bool) func() {
		return func() {
			txMu.Lock()
			for _, m := range txMsgs {
				m.running = on && m.period > 0
				m.lastAt = time.Time{}
			}
			txMu.Unlock()
			txList.Refresh()
		}
	}
	txControls := container.NewVBox(
		container.NewHBox(
			widget.NewLabel("ID:"), minWidth(txID, 90),
			widget.NewLabel("Data:"), minWidth(txData, 230),
			widget.NewLabel("Period (ms):"), minWidth(txPeriod, 70),
			txExt,
		),
		container.NewHBox(txSingle, txAdd, txDelete,
			widget.NewButton("Send All", txAll(true)), widget.NewButton("Stop All", txAll(false))),
	)
	go txScheduler()

	includeEntry := widget.NewMultiLineEntry()
	includeEntry.SetPlaceHolder("hex IDs, e.g. 7E8 0x123 1FF — empty = all")
	excludeEntry := widget.NewMultiLineEntry()
	excludeEntry.SetPlaceHolder("hex IDs to drop")
	applyFilter := func(string) {
		inc, err := parseIDs(includeEntry.Text)
		if err == nil {
			var exc map[uint32]bool
			if exc, err = parseIDs(excludeEntry.Text); err == nil {
				filterPtr.Store(&filterSet{include: inc, exclude: exc})
				status.SetText(fmt.Sprintf("Filter: %d include, %d exclude", len(inc), len(exc)))
				return
			}
		}
		status.SetText("Filter error: " + err.Error()) // keep last good filter
	}
	includeEntry.OnChanged = applyFilter
	excludeEntry.OnChanged = applyFilter
	filterTab := container.NewGridWithColumns(2,
		container.NewBorder(widget.NewLabel("Include"), nil, nil, nil, includeEntry),
		container.NewBorder(widget.NewLabel("Exclude"), nil, nil, nil, excludeEntry),
	)

	var cancel context.CancelFunc // nil = disconnected; only touched on the Fyne thread

	connectBtn := widget.NewButton("Connect", nil)
	connectBtn.OnTapped = func() {
		if cancel != nil {
			cancel()
			return
		}
		rate, err := strconv.ParseFloat(strings.TrimSpace(rateEntry.Text), 64)
		if err != nil {
			status.SetText("bad CAN rate: " + rateEntry.Text)
			return
		}
		adapter, port := adapterSel.Selected, portEntry.Text
		saveSettings(a.Preferences(), adapter, port, rateEntry.Text)
		connectBtn.Disable()
		status.SetText("Connecting...")

		go func() {
			ctx, c := context.WithCancel(context.Background())
			bus, err := gocan.Open(ctx, adapter, gocan.Config{Port: port, CANRate: rate},
				gocan.WithEventFunc(func(e gocan.Event) {
					if e.Err != nil {
						logf("gocan %s: %v", e, e.Err)
					} else {
						logf("gocan %s", e)
					}
				}))
			if err != nil {
				c()
				logf("connect %s %s: %v", adapter, port, err)
				fyne.Do(func() {
					status.SetText("Error: " + err.Error())
					connectBtn.Enable()
				})
				return
			}
			logf("connected %s on %s @ %gkbit", adapter, port, rate)
			txBus.Store(bus)
			fyne.Do(func() {
				cancel = c
				status.SetText(fmt.Sprintf("Connected: %s @ %gkbit", adapter, rate))
				connectBtn.SetText("Disconnect")
				connectBtn.Enable()
			})
			for f := range bus.Frames(ctx) {
				record(f)
			}
			txBus.Store(nil)
			err = bus.Err()
			bus.Close()
			c()
			if err != nil {
				logf("bus closed: %v", err)
			} else {
				logf("disconnected")
			}
			fyne.Do(func() {
				cancel = nil
				if err != nil {
					status.SetText("Error: " + err.Error())
				} else {
					status.SetText("Disconnected")
				}
				connectBtn.SetText("Connect")
				connectBtn.Enable()
			})
		}()
	}

	clearBtn := widget.NewButton("Clear", func() {
		statsMu.Lock()
		clear(stats)
		trace = nil
		statsMu.Unlock()
		rows = nil
		traceRows = nil
		table.Refresh()
		traceList.Refresh()
	})

	go func() {
		for range time.Tick(200 * time.Millisecond) {
			snap, tsnap := snapshot()
			logMu.Lock()
			lsnap := slices.Clone(logLines)
			logMu.Unlock()
			fyne.Do(func() {
				rows = snap
				table.Refresh()
				if len(tsnap) != len(traceRows) {
					traceRows = tsnap
					traceList.Refresh()
					if autoscroll.Checked {
						traceList.ScrollToBottom()
					}
				}
				txList.Refresh()
				if len(lsnap) != len(logRows) {
					logRows = lsnap
					logList.Refresh()
					logList.ScrollToBottom()
				}
			})
		}
	}()

	top := container.NewHBox(
		widget.NewLabel("Adapter:"), minWidth(adapterSel, adapterW),
		widget.NewLabel("Port:"), minWidth(portEntry, 160), refreshBtn,
		widget.NewLabel("Rate:"), minWidth(rateEntry, 110),
		connectBtn, clearBtn,
	)
	tabs := container.NewAppTabs(
		container.NewTabItem("Monitor", table),
		container.NewTabItem("Trace", container.NewBorder(container.NewHBox(autoscroll, recordBtn), nil, nil, nil,
			container.NewThemeOverride(traceList, compactTheme{theme.DefaultTheme()}))),
		container.NewTabItem("Transmit", container.NewBorder(nil, txControls, nil, nil,
			container.NewThemeOverride(txList, compactTheme{theme.DefaultTheme()}))),
		container.NewTabItem("Filter", filterTab),
		//ontainer.NewTabItem("Log", container.NewThemeOverride(logList, compactTheme{theme.DefaultTheme()})),
	)
	w.SetContent(container.NewBorder(
		top,
		container.NewBorder(
			nil,
			status,
			nil,
			nil,
			minHeight(container.NewThemeOverride(logList, compactTheme{theme.DefaultTheme()}), 75),
		),
		//status,
		nil,
		nil,
		tabs))
	w.Resize(fyne.NewSize(700, 500))
	if *screenshotDir != "" {
		go captureTabs(a, w, tabs, *screenshotDir)
	}
	w.ShowAndRun()
}
