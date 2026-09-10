package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/joho/godotenv"
)

func runAPIKeygen() int {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		fmt.Fprintf(os.Stderr, "Error generating secure key: %v\n", err)
		return 1
	}
	key := base64.RawURLEncoding.EncodeToString(b)
	fmt.Println(key)
	return 0
}

func resolveAPIKey(explicitKey string, explicitKeySet bool, inheritedKey string, inheritedKeySet bool, envPath string) (string, error) {
	// Precedence 1: Explicit --api-key argument
	if explicitKeySet {
		if explicitKey == "" {
			return "", errors.New("API key provided via --api-key cannot be empty")
		}
		if len([]byte(explicitKey)) < 32 {
			return "", errors.New("API key provided via --api-key must be at least 32 bytes")
		}
		return explicitKey, nil
	}

	// Precedence 2: Inherited OS environment variable OPENAI_COMPAT_API_KEY
	if inheritedKeySet {
		if inheritedKey == "" {
			return "", errors.New("OPENAI_COMPAT_API_KEY environment variable cannot be empty")
		}
		if len([]byte(inheritedKey)) < 32 {
			return "", errors.New("OPENAI_COMPAT_API_KEY environment variable must be at least 32 bytes")
		}
		return inheritedKey, nil
	}

	// Precedence 3: Selected .env file
	if envMap, err := godotenv.Read(envPath); err == nil {
		if val, ok := envMap["OPENAI_COMPAT_API_KEY"]; ok {
			if val == "" {
				return "", errors.New("OPENAI_COMPAT_API_KEY in .env cannot be empty")
			}
			if len([]byte(val)) < 32 {
				return "", errors.New("OPENAI_COMPAT_API_KEY in .env must be at least 32 bytes")
			}
			return val, nil
		}
	}

	return "", errors.New("OPENAI_COMPAT_API_KEY is required when --openai-api is enabled")
}

// main is the entry point only: CLI flags, single-instance lock, log file,
// signal handling and shutdown. All dependency assembly lives in
// bootstrap.go and all runtime behavior in the Application/Controller layers.
func main() {
	// Capture inherited process environment variable before any dotenv loading
	inheritedAPIKey, inheritedAPIKeySet := os.LookupEnv("OPENAI_COMPAT_API_KEY")

	portPtr := flag.Int("port", 49152, "Port number to use for single instance lock / API server")
	envPtr := flag.String("env", "", "Path to the .env file (default: <executable dir>/../src/.env)")
	telegramProxyPtr := flag.String("telegram-proxy", "", "Proxy URL for Telegram API (http://, https://, socks5://, or socks5h://)")
	cronDisabledPtr := flag.Bool("cron-disabled", false, "Disable the /cron scheduled-task subsystem entirely")
	openaiApiPtr := flag.Bool("openai-api", false, "Enable loopback OpenAI-compatible API adaptor")
	apiKeyPtr := flag.String("api-key", "", "API key for OpenAI-compatible API (non-preferred; see documentation)")
	apiKeygenPtr := flag.Bool("api-keygen", false, "Generate a random 32-byte API key, print to stdout, and exit")
	flag.Parse()

	var apiKeyExplicitlySet bool
	var apiKeygenExplicitlySet bool
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "api-key" {
			apiKeyExplicitlySet = true
		} else if f.Name == "api-keygen" {
			apiKeygenExplicitlySet = true
		}
	})

	if *apiKeygenPtr || apiKeygenExplicitlySet {
		if flag.NFlag() > 1 {
			fmt.Fprintln(os.Stderr, "Error: --api-keygen must be the only flag supplied")
			os.Exit(2)
		}
		os.Exit(runAPIKeygen())
	}

	if apiKeyExplicitlySet && !*openaiApiPtr {
		fmt.Fprintln(os.Stderr, "Error: --api-key requires --openai-api")
		os.Exit(2)
	}

	exePathForLog, _ := os.Executable()
	logDir := filepath.Dir(exePathForLog)
	envPath := resolveEnvPath(*envPtr, logDir)

	var effectiveAPIKey string
	if *openaiApiPtr {
		key, err := resolveAPIKey(*apiKeyPtr, apiKeyExplicitlySet, inheritedAPIKey, inheritedAPIKeySet, envPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		effectiveAPIKey = key
	}

	lockAddr := fmt.Sprintf("127.0.0.1:%d", *portPtr)
	listener, err := net.Listen("tcp", lockAddr)
	if err != nil {
		fmt.Printf("Error: gemini-connector is already running (failed to bind to port %s).\n", lockAddr)
		os.Exit(1)
	}
	defer listener.Close()

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

	var apiServer *OpenAICompatibleServer
	var httpServer *http.Server
	if *openaiApiPtr {
		apiLogger := NewAPILogger(logDir)
		apiServer = NewOpenAICompatibleServer(effectiveAPIKey, app.turns, apiLogger)
		httpServer = &http.Server{
			Handler:           apiServer,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Second,
		}
		httpServer.BaseContext = func(l net.Listener) context.Context { return apiServer.rootCtx }

		go func() {
			log.Printf("OpenAI-compatible API adapter listening on http://%s", lockAddr)
			if serveErr := httpServer.Serve(listener); serveErr != nil && serveErr != http.ErrServerClosed {
				log.Fatalf("OpenAI API Server error: %v", serveErr)
			}
		}()
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

		if *openaiApiPtr && apiServer != nil {
			apiServer.Drain()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = httpServer.Shutdown(shutdownCtx)
			cancel()
		}
		listener.Close()
		app.turns.StopAll()
		stopOnce.Do(func() { close(stopCh) })
	}()

	app.Run(stopCh)

	listener.Close()
}
