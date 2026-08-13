# Gemini Telegram Connector (Event-Driven Controller)

> "Openclaw for the Poor"

This project is a standalone Telegram connector program written in Go, acting as an **Event-Driven Controller** bridging Telegram and **agy (Antigravity CLI)**. 
It uses a lightweight, highly stable event-driven model where the AI is only woken up (triggered) when a Telegram message is received.

## Prerequisites

To build and run this connector, the following software must be installed on your system:
-   **Go:** Required to compile the connector's source code. (v1.25+ recommended)
-   **Git:** Required to download Go package dependencies.
-   **agy (Antigravity CLI):** The actual AI agent that the connector triggers in the background.
    -   Windows: `irm https://antigravity.google/cli/install.ps1 | iex`
    -   Linux/macOS: `curl -fsSL https://antigravity.google/cli/install.sh | bash`

## Features

-   **Event-Driven Architecture:** The connector handles Telegram long-polling exclusively. Upon receiving a message, it triggers `agy` in the background via `os/exec`. (No infinite waiting loops for the AI).
-   **Stateful Sessions:** Utilizes agy's `--conversation <ID>` feature. Even though the AI process starts and stops for every message, the conversational context is perfectly preserved.
-   **Markdown → Telegram HTML Conversion:** Automatically converts agy's Markdown responses into Telegram-compatible HTML. Bold/italic/inline code/code blocks/links/quotes are converted to tags, while headings, lists, and images degrade to Telegram-supported forms. If HTML sending fails, it automatically falls back to plain text.
-   **Media Album Buffering:** When multiple photos or files are sent simultaneously via Telegram (as an album), a 2-second debounce buffer collects them into a single, unified prompt for the AI.
-   **Intelligent Retry & Resilience:** Detects `429 Too Many Requests` (Rate Limit) errors from the Telegram API, parses the `Retry-After` headers, and safely waits before retrying.
-   **Externalized Messaging:** All welcome messages and error text are managed externally in a `messages.json` file, allowing easy customization without recompiling the source code.
-   **Interactive Conversation Helper (TUI):** If `AGY_CONVERSATION_ID` is missing in `.env`, the connector automatically reads agy's local conversation cache (`~/.gemini/antigravity-cli/cache/last_conversations.json`) and displays workspace-based conversations in a paginated list (10 per page). Users can pick a number to link a conversation, press `[c]` to create a new one, or `[m]` to enter an ID manually.

## Directory Structure

Source code, executables, and data are clearly separated.

```text
[Project Root]/
├── .gemini/             # Shared Gemini/agy configuration folder
│   └── settings.json    # agy configuration file
└── golang/gemini-connector/
    ├── src/
    │   ├── main.go            # Core connector source code
    │   ├── gemini.go          # agy CLI invocation and JSON response parsing
    │   ├── session.go         # Interactive conversation selector TUI + new conversation creation
    │   ├── telegram.go        # Telegram adapter (long-polling, media, chunking)
    │   ├── telegram_html.go   # Markdown → Telegram HTML conversion (goldmark)
    │   ├── teams.go           # Teams adapter (optional)
    │   ├── messenger.go       # Messenger common interface and adapter routing
    │   ├── go.mod             # Go module dependencies
    │   ├── .env               # Environment variables (Token, Chat ID, Conversation ID) - Referenced at runtime
    │   └── messages.json      # Externalized UI/error messages - Referenced at runtime
    ├── bin/
    │   ├── gemini-connector_windows_x64.exe # Compiled executable (Windows 64-bit)
    │   ├── gemini-connector_linux_x64       # Compiled executable (Linux 64-bit)
    │   └── bot.log            # Execution and error logs generated during runtime
    └── downloads/             # Temporary storage for media files (images, audio) received via Telegram
```

## Setup

1.  **Obtain Telegram Bot Token:**
    -   Talk to `@BotFather` on Telegram to create a new bot and obtain the `TELEGRAM_BOT_TOKEN`.

2.  **Install & Authenticate agy:**
    -   Install agy via the official install script. (Windows: `irm https://antigravity.google/cli/install.ps1 | iex`)
    -   Run `agy` once in a terminal to complete login authentication.

3.  **Conversation Setup (Conversation ID):**
    -   **Automatic Setup (Recommended):** When you run the connector, an interactive TUI helper will automatically list cached conversations. Simply enter the number of the conversation you want to use, and the ID will be saved to `.env` automatically. Press `[c]` to create a new conversation.
    -   **Manual Setup:** Create a conversation via agy in a terminal and enter the returned `conversation_id` manually.

4.  **Configuration (.env):**
    -   Create a `.env` file inside the `src/` folder, or run the connector once to trigger the setup wizard.
    -   Enter your Telegram Bot Token, and then follow the **Conversation Selection TUI** to pick your agy conversation.
    ```ini
    ACTIVE_MESSENGERS=telegram
    TELEGRAM_BOT_TOKEN=your_telegram_bot_token
    TELEGRAM_CHAT_ID=your_chat_id
    AGY_CONVERSATION_ID=auto_or_manually_entered_id
    ```

## Installation & Run

You can either compile the source code yourself or download a pre-built executable to get started immediately.

### Option 1: Download from GitHub Releases (Recommended)
1. Go to the [Releases] page of this repository.
2. Download the appropriate executable for your operating system (e.g., `gemini-connector_windows_x64.exe`) and place it in the `golang/gemini-connector/bin/` directory or any desired location.

### Option 2: Build from Source
If you have Go installed, you can compile the source code directly.

```bash
cd golang/gemini-connector/src
go build -ldflags="-s -w" -o ../bin/gemini-connector_windows_x64.exe
```

### Run
Simply run the compiled or downloaded executable as a standalone background process.

```bash
cd golang/gemini-connector/bin
./gemini-connector_windows_x64.exe
```
(Tip: Use `Start-Process` (Windows) or `nohup` (Linux) or other background execution tools to keep the connector running continuously without blocking the foreground shell.)

## ⚠️ Disclaimer and Risk Warning

This connector explicitly executes **agy with the `--dangerously-skip-permissions` flag** to achieve full automation and convenience. This means that all tool invocations and local file system controls (modifications, deletions, etc.) determined by the AI will be executed immediately **without requiring any prior user confirmation**.

*   The developers and contributors of this project assume **absolutely no liability** for any direct or indirect damages, data loss, unauthorized system file modifications, or security vulnerabilities that may arise from the AI's hallucinations or incorrect judgments.
*   **All risks and responsibilities associated with executing this code and utilizing its outputs rest entirely with the user.** It is strongly recommended to run this connector within a secure sandbox environment or with restricted permissions.
