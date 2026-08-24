package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// main is the entry point only: CLI flags, single-instance lock, log file,
// signal handling and shutdown. All dependency assembly lives in
// bootstrap.go and all runtime behavior in the Application/Controller layers.
func main() {
	portPtr := flag.Int("port", 49152, "Port number to use for single instance lock")
	envPtr := flag.String("env", "", "Path to the .env file (default: <executable dir>/../src/.env)")
	telegramProxyPtr := flag.String("telegram-proxy", "", "Proxy URL for Telegram API (http://, https://, socks5://, or socks5h://)")
	cronDisabledPtr := flag.Bool("cron-disabled", false, "Disable the /cron scheduled-task subsystem entirely")
	flag.Parse()

	lockAddr := fmt.Sprintf("127.0.0.1:%d", *portPtr)
	listener, err := net.Listen("tcp", lockAddr)
	if err != nil {
		fmt.Printf("Error: gemini-connector is already running (failed to bind to port %s).\n", lockAddr)
		os.Exit(1)
	}
	defer listener.Close()

	exePathForLog, _ := os.Executable()
	logDir := filepath.Dir(exePathForLog)

	logPath := filepath.Join(logDir, "bot.log")
	logFile, logErr := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0666)
	if logErr == nil {
		defer logFile.Close()
		log.SetOutput(logFile)

		// 5분 주기 로그 플러시 (비정상 종료 시 유실 최소화)
		go func() {
			ticker := time.NewTicker(5 * time.Minute)
			defer ticker.Stop()
			for range ticker.C {
				logFile.Sync()
			}
		}()
	} else {
		log.SetOutput(os.Stderr)
	}

	// 시그널 핸들링: 정상 종료 시 로그 플러시 보장
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	stopCh := make(chan struct{})
	var stopOnce sync.Once
	go func() {
		sig := <-sigChan
		log.Printf("Received signal: %v. Shutting down...", sig)
		if logFile != nil {
			logFile.Sync()
		}
		listener.Close()
		stopOnce.Do(func() { close(stopCh) })
	}()

	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
	log.Println("Starting Gemini Connector...")

	app, err := Bootstrap(BootstrapOptions{
		EnvFlag:       *envPtr,
		TelegramProxy: *telegramProxyPtr,
		CronDisabled:  *cronDisabledPtr,
	})
	if err != nil {
		log.Fatalf("Startup Error: %v", err)
	}

	app.Run(stopCh)

	listener.Close()
}
