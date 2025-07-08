package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/paragor/dead-mans-switch/internal/envflag"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"gopkg.in/telebot.v3"
)

type Config struct {
	ListenAddr        string
	MetricsListenAddr string
	Token             string
	Hostname          string

	FirstPingWaitTimeout      time.Duration
	BetweenPingWaitTimeout    time.Duration
	InformMessagePeriod       time.Duration
	InFireInformMessagePeriod time.Duration
	Telegram                  struct {
		Token  string
		ChatId int
	}
}

func initConfig(_ context.Context) *Config {
	cnf := &Config{
		FirstPingWaitTimeout:      time.Minute,
		BetweenPingWaitTimeout:    time.Minute,
		InformMessagePeriod:       31 * 24 * time.Hour,
		InFireInformMessagePeriod: time.Hour,
	}
	envflag.StringVar(&cnf.ListenAddr, "listen-addr", "0.0.0.0:8080", "listen web hook address")
	envflag.StringVar(&cnf.MetricsListenAddr, "metrics-listen-addr", "127.0.0.1:7070", "listen metrics address")
	envflag.StringVar(&cnf.Token, "webhook-token", "", "web hook token")
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}
	envflag.StringVar(&cnf.Hostname, "hostname", hostname, "hostname to identify watchdog")

	envflag.StringVar(&cnf.Telegram.Token, "telegram-token", "", "telegram token for send notifications")
	envflag.IntVar(&cnf.Telegram.ChatId, "telegram-chat-id", 0, "telegram destination for notification")
	slog.Info(
		"You can use env variable for every flag",
		slog.String("mapping", fmt.Sprintf("%v", envflag.KnownEnvs())),
	)

	envflag.Parse()
	return cnf
}

func initChat(_ context.Context, cnf *Config, bot *telebot.Bot) (*telebot.Chat, error) {
	if cnf.Telegram.ChatId == 0 {
		return nil, fmt.Errorf("telegram chat id is not set")
	}
	chat, err := bot.ChatByID(int64(cnf.Telegram.ChatId))
	if err != nil {
		return nil, fmt.Errorf("cant get Telegram chat id: %w", err)
	}
	return chat, nil
}

func initTelegram(_ context.Context, cnf *Config) (*telebot.Bot, error) {
	if cnf.Telegram.Token == "" {
		return nil, fmt.Errorf("telegram token is not set")
	}
	bot, err := telebot.NewBot(telebot.Settings{
		Token: cnf.Telegram.Token,
		Client: &http.Client{
			Transport:     http.DefaultTransport,
			CheckRedirect: nil,
			Jar:           nil,
			Timeout:       time.Minute * 5,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("cant create telegram client: %w", err)
	}
	return bot, nil
}

func main() {
	mainCtx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	cnf := initConfig(mainCtx)
	bot, err := initTelegram(mainCtx, cnf)
	if err != nil {
		log.Fatal(err.Error())
	}
	chat, err := initChat(mainCtx, cnf, bot)
	if err != nil {
		log.Fatal(err.Error())
	}

	watchdog, watchdogErrChan := startWatchdog(mainCtx, cnf, bot, chat)
	metricsServerErrors := startMetricsServer(mainCtx, cnf)
	webhookServerErrors := startWebhookServer(mainCtx, cnf, watchdog)

	select {
	case err := <-watchdogErrChan:
		if err != nil {
			log.Fatalf("watchdog error: %s", err.Error())
		} else {
			log.Fatal("watchdog stopped")
		}
	case err := <-metricsServerErrors:
		if err != nil {
			log.Fatalf("metrics server error: %s", err.Error())
		} else {
			log.Fatal("metrics server stopped")
		}
	case err := <-webhookServerErrors:
		if err != nil {
			log.Fatalf("webhook server error: %s", err.Error())
		} else {
			log.Fatal("webhook server stopped")
		}
	case <-mainCtx.Done():
		slog.Info("receive shutdown message...")
	}

	wg := sync.WaitGroup{}
	for name, errChan := range map[string]<-chan error{
		"watchdog":       watchdogErrChan,
		"metrics server": metricsServerErrors,
		"webhook server": webhookServerErrors,
	} {
		myErrChan := errChan
		myName := name
		wg.Add(1)
		go func() {
			for err := range myErrChan {
				if err == nil {
					continue
				}
				slog.Error(myName+" has error", slog.String("err", err.Error()))
			}
			wg.Done()
		}()
	}
	wgDone := make(chan bool)
	go func() {
		wg.Wait()
		wgDone <- true
	}()

	shutdownTimeout := time.NewTimer(time.Second * 15)
	select {
	case <-shutdownTimeout.C:
		log.Fatal("Force shutdown")
	case <-wgDone:
		log.Println("Graceful shutdown")
	}
}

func startWebhookServer(ctx context.Context, cnf *Config, w *WatchDog) <-chan error {
	errChan := make(chan error, 1)
	if cnf.Token == "" {
		errChan <- fmt.Errorf("webhook token can not be empty")
		close(errChan)
		return errChan
	}
	mux := &http.ServeMux{}
	webhookEndpoint := "GET /webhook/" + cnf.Token
	mux.Handle(webhookEndpoint, w)
	server := &http.Server{
		Addr:    cnf.ListenAddr,
		Handler: mux,
	}
	go func() {
		slog.Info(
			"listen main server",
			slog.String("listen", cnf.ListenAddr),
			slog.String("endpoint", webhookEndpoint),
		)
		err := server.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errChan <- err
		}
		close(errChan)
	}()
	go func() {
		select {
		case <-ctx.Done():
			shutdownContext, cancel := context.WithTimeout(context.Background(), time.Second*10)
			defer cancel()
			err := server.Shutdown(shutdownContext)
			if err != nil {
				slog.Warn("webhook server shutdown with err", slog.String("err", err.Error()))
			}
		}
	}()
	return errChan
}

func startMetricsServer(ctx context.Context, cnf *Config) <-chan error {
	errChan := make(chan error, 1)
	mux := &http.ServeMux{}
	endpoint := "GET /metrics"
	mux.Handle(endpoint, promhttp.Handler())
	server := &http.Server{
		Addr:    cnf.MetricsListenAddr,
		Handler: mux,
	}

	go func() {
		slog.Info(
			"listen metrics server",
			slog.String("listen", cnf.ListenAddr),
			slog.String("endpoint", endpoint),
		)

		err := server.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errChan <- err
		}
		close(errChan)
	}()
	go func() {
		select {
		case <-ctx.Done():
			shutdownContext, cancel := context.WithTimeout(context.Background(), time.Second*10)
			defer cancel()
			err := server.Shutdown(shutdownContext)
			if err != nil {
				slog.Warn("metrics server shutdown with err", slog.String("err", err.Error()))
			}
		}
	}()
	return errChan
}

type WatchDog struct {
	bot       *telebot.Bot
	chat      *telebot.Chat
	startTime time.Time
	hostname  string

	mainMutex   sync.Mutex
	informMutex sync.Mutex
	fireMutex   sync.Mutex

	lastPing    time.Time
	anyPingDone bool
	inFireState bool

	lastSendingMessageTime time.Time
	fireStart              time.Time

	firstPingWaitTimeout      time.Duration
	betweenPingWaitTimeout    time.Duration
	informMessagePeriod       time.Duration
	inFireInformMessagePeriod time.Duration

	errChan       chan error
	errChanClosed bool
	errChanMutex  sync.Mutex
}

func (w *WatchDog) shutdown(err error) {
	w.errChanMutex.Lock()
	defer w.errChanMutex.Unlock()
	if w.errChanClosed {
		return
	}
	if err != nil {
		w.errChan <- err
	}
	close(w.errChan)
}

func (w *WatchDog) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	w.mainMutex.Lock()
	w.anyPingDone = true
	w.lastPing = time.Now()
	w.mainMutex.Unlock()

	slog.Info("receive ping", slog.String("remote_addr", request.RemoteAddr))
	if err := w.CalmDown(context.Background(), "Right now all is ok"); err != nil {
		w.shutdown(fmt.Errorf("cant send success message: %w", err))
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(200)
	_, _ = writer.Write([]byte(`{"status": "ok"}`))
}

func (w *WatchDog) send(_ context.Context, msg string) error {
	msg = fmt.Sprintf("[%s]: %s", w.hostname, msg)
	slog.Info("send telegram message", slog.String("msg", msg))
	_, err := w.bot.Send(w.chat, msg)
	if err != nil {
		return fmt.Errorf("cant send telegram message: %w", err)
	}
	w.informMutex.Lock()
	w.lastSendingMessageTime = time.Now()
	w.informMutex.Unlock()
	return nil
}

func (w *WatchDog) getTimeInFire() (bool, time.Duration) {
	w.fireMutex.Lock()
	inFire := w.inFireState
	fireStart := w.fireStart
	defer w.fireMutex.Unlock()
	if !inFire {
		return false, 0
	}
	return true, time.Now().Sub(fireStart)
}

func (w *WatchDog) Fire(ctx context.Context, msg string) error {
	w.fireMutex.Lock()
	if w.inFireState {
		w.fireMutex.Unlock()
		return nil
	}
	w.inFireState = true
	w.fireStart = time.Now()
	w.fireMutex.Unlock()
	return w.send(ctx, "🚨🐺 ALARM! "+msg)
}
func (w *WatchDog) CalmDown(ctx context.Context, msg string) error {
	w.fireMutex.Lock()
	if !w.inFireState {
		w.fireMutex.Unlock()
		return nil
	}
	w.inFireState = false
	w.fireMutex.Unlock()
	return w.send(ctx, "✅🐺 GOOD! "+msg)
}

func (w *WatchDog) startInformLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Second * 5)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.informMutex.Lock()
			lastSendingMessageTime := w.lastSendingMessageTime
			w.informMutex.Unlock()
			if lastSendingMessageTime.IsZero() {
				continue
			}
			if time.Now().After(lastSendingMessageTime.Add(w.informMessagePeriod)) {
				if err := w.send(ctx, "info. jfyi information watchdog working fine :)"); err != nil {
					w.shutdown(fmt.Errorf("inform loop: cant send information message: %w", err))
					return
				}
			}

			if inFire, timeInFire := w.getTimeInFire(); inFire && timeInFire > w.inFireInformMessagePeriod {
				if err := w.send(
					ctx,
					fmt.Sprintf("info. jfyi information watchdog still if fire for %s ...", timeInFire),
				); err != nil {
					w.shutdown(fmt.Errorf("inform loop: cant send information about fire: %w", err))
					return
				}
			}
		}
	}
}

func (w *WatchDog) startMainLoop(ctx context.Context) {
	if err := w.send(ctx, "Watchdog is starting now!"); err != nil {
		w.shutdown(fmt.Errorf("error on send greating message: %w", err))
		return
	}

	ticker := time.NewTicker(time.Second * 5)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			shutdownContext, cancel := context.WithTimeout(context.Background(), time.Second*10)
			if err := w.send(shutdownContext, "Watchdog have been stopped..."); err != nil {
				w.shutdown(fmt.Errorf("error on send stopped message: %w", err))
			}
			w.shutdown(nil)
			cancel()
			return
		case <-ticker.C:
			w.mainMutex.Lock()
			lastPing := w.lastPing
			anyPingDone := w.anyPingDone
			w.mainMutex.Unlock()

			if !anyPingDone {
				if time.Now().Before(w.startTime.Add(w.firstPingWaitTimeout)) {
					slog.Info("still wait for first ping", slog.Duration("time_after_start", time.Now().Sub(w.startTime)))
					continue
				}
				if err := w.Fire(ctx, "no ping after start watchdog"); err != nil {
					w.shutdown(fmt.Errorf("cant send 'first ping timeout alert': %w", err))
					return
				}
				continue
			}

			if time.Now().After(lastPing.Add(w.betweenPingWaitTimeout)) {
				if err := w.Fire(ctx, "no ping for a long time"); err != nil {
					w.shutdown(fmt.Errorf("cant send 'first ping timeout alert': %w", err))
					return
				}
				continue
			}
		}
	}
}

func startWatchdog(ctx context.Context, cnf *Config, bot *telebot.Bot, chat *telebot.Chat) (*WatchDog, <-chan error) {
	errChan := make(chan error, 1)
	w := &WatchDog{
		anyPingDone:               false,
		lastPing:                  time.Time{},
		bot:                       bot,
		chat:                      chat,
		startTime:                 time.Now(),
		firstPingWaitTimeout:      cnf.FirstPingWaitTimeout,
		betweenPingWaitTimeout:    cnf.BetweenPingWaitTimeout,
		informMessagePeriod:       cnf.InformMessagePeriod,
		hostname:                  cnf.Hostname,
		inFireInformMessagePeriod: cnf.InFireInformMessagePeriod,

		errChan: errChan,
	}
	go w.startMainLoop(ctx)
	go w.startInformLoop(ctx)
	return w, errChan
}
