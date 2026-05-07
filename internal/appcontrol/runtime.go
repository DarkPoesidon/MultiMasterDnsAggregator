package appcontrol

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/DarkPoesidon/MultiMasterDnsAggregator/internal/multipath"
)

type RuntimeStatus struct {
	Running        bool
	HealthyBearers int
	TotalBearers   int
	ActiveStreams  int
	LastError      string
}

type Runtime struct {
	mu         sync.RWMutex
	cfg        AppConfig
	log        *bufferedLogger
	manager    *multipath.MultipathManager
	dispatcher *multipath.MultipathDispatcher
	cancel     context.CancelFunc
	running    bool
	lastErr    string
}

func NewRuntime(cfg AppConfig) *Runtime {
	if cfg.ListenAddr == "" {
		cfg = DefaultAppConfig()
	}
	return &Runtime{
		cfg: cfg,
		log: newBufferedLogger(800),
	}
}

func (r *Runtime) Start() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.running {
		return nil
	}
	if err := r.cfg.Validate(); err != nil {
		return err
	}

	mpCfg := r.cfg.ToMultipathConfig()
	ctx, cancel := context.WithCancel(context.Background())
	mgr := multipath.NewMultipathManager(mpCfg, r.log)
	disp := multipath.NewMultipathDispatcher(mpCfg, mgr, r.log)
	r.lastErr = ""

	mgr.Start(ctx)

	go func() {
		err := disp.Run(ctx)
		if err != nil {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.lastErr = err.Error()
		}
	}()

	r.cancel = cancel
	r.manager = mgr
	r.dispatcher = disp
	r.running = true
	r.log.Infof("runtime started on %s", r.cfg.ListenAddr)

	return nil
}

func (r *Runtime) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.running {
		return
	}

	if r.cancel != nil {
		r.cancel()
	}
	if r.dispatcher != nil {
		r.dispatcher.Stop()
	}
	if r.manager != nil {
		r.manager.Stop()
	}

	r.running = false
	r.log.Infof("runtime stopped")
}

func (r *Runtime) UpdateConfig(cfg AppConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.running {
		return fmt.Errorf("cannot update config while running")
	}
	r.cfg = cfg
	return nil
}

func (r *Runtime) Config() AppConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cfg
}

func (r *Runtime) Status() RuntimeStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()

	st := RuntimeStatus{
		Running:   r.running,
		LastError: r.lastErr,
	}
	if r.manager != nil && r.running {
		st.HealthyBearers, st.TotalBearers = r.manager.HealthStatus()
		st.ActiveStreams = r.manager.ActiveStreamCount()
	}
	return st
}

func (r *Runtime) RecentLogs(max int) []string {
	return r.log.Recent(max)
}

type bufferedLogger struct {
	mu      sync.Mutex
	max     int
	entries []string
}

func newBufferedLogger(max int) *bufferedLogger {
	return &bufferedLogger{
		max:     max,
		entries: make([]string, 0, max),
	}
}

func (l *bufferedLogger) append(level, format string, args ...any) {
	line := fmt.Sprintf("%s %-5s %s", time.Now().Format(time.RFC3339), level, fmt.Sprintf(format, args...))

	l.mu.Lock()
	defer l.mu.Unlock()

	l.entries = append(l.entries, line)
	if len(l.entries) > l.max {
		l.entries = l.entries[len(l.entries)-l.max:]
	}
}

func (l *bufferedLogger) Debugf(format string, args ...any) { l.append("DEBUG", format, args...) }
func (l *bufferedLogger) Infof(format string, args ...any)  { l.append("INFO", format, args...) }
func (l *bufferedLogger) Warnf(format string, args ...any)  { l.append("WARN", format, args...) }
func (l *bufferedLogger) Errorf(format string, args ...any) { l.append("ERROR", format, args...) }

func (l *bufferedLogger) Recent(max int) []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	if max <= 0 || max >= len(l.entries) {
		out := make([]string, len(l.entries))
		copy(out, l.entries)
		return out
	}

	out := make([]string, max)
	copy(out, l.entries[len(l.entries)-max:])
	return out
}

func FormatStatusLine(st RuntimeStatus) string {
	if !st.Running {
		if st.LastError != "" {
			return "Stopped (error: " + st.LastError + ")"
		}
		return "Stopped"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Running | bearers %d/%d healthy | active streams %d",
		st.HealthyBearers, st.TotalBearers, st.ActiveStreams)
	if st.LastError != "" {
		fmt.Fprintf(&b, " | last error: %s", st.LastError)
	}
	return b.String()
}
