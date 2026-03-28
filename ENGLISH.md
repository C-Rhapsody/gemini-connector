# Gemini Telegram Connector (Event-Driven Controller)

> "Openclaw for the Poor"

This project is a standalone Telegram connector program written in Go, acting as an **Event-Driven Controller** bridging Telegram and **Gemini CLI**. 
It uses a lightweight, highly stable event-driven model where the AI is only woken up (triggered) when a Telegram message is received.

## Prerequisites

To build and run this connector, the following software must be installed on your system:
-   **Go:** Required to compile the connector's source code. (v1.25+ recommended)
-   **Git:** Required to download Go package dependencies.
-   **Gemini CLI:** The actual AI agent that the connector triggers in the background. (e.g., install via `npm install -g @google/gemini-cli`)

## Features

-   **Event-Driven Architecture:** The connector handles Telegram long-polling exclusively. Upon receiving a message, it triggers `gemini-cli` in the background via `os/exec`. (No infinite waiting loops for the AI).
-   **Stateful Sessions:** Utilizes Gemini CLI's `--resume <UUID>` feature. Even though the AI process starts and stops for every message, the conversational context is perfectly preserved.
-   **Media Album Buffering:** When multiple photos or files are sent simultaneously via Telegram (as an album), a 2-second debounce buffer collects them into a single, unified prompt for the AI.
-   **Intelligent Retry & Resilience:** Detects `429 Too Many Requests` (Rate Limit) errors from the Telegram API, parses the `Retry-After` headers, and safely waits before retrying.
-   **Externalized Messaging:** All welcome messages and error text are managed externally in a `messages.json` file, allowing easy customization without recompiling the source code.

## Directory Structure

Following the new architecture, source code, executables, and data are clearly separated.

```text
[Project Root]/
├── .gemini/             # Folder where Gemini CLI global settings and session data are stored
│   ├── settings.json    # Gemini CLI configuration file
│   ├── gemini.md        # Core system prompt and operational guidelines for the AI
│   └── personality.md   # AI persona (identity and tone) configuration file
└── golang/gemini-connector/
    ├── src/
    │   ├── main.go          # Core connector source code
    │   ├── go.mod           # Go module dependencies
    │   ├── .env             # Environment variables (Token, Chat ID, UUID) - Referenced at runtime
    │   └── messages.json    # Externalized UI/error messages - Referenced at runtime
    ├── bin/
    │   ├── gemini-connector.exe # Compiled standalone executable (Run the connector from here)
    │   └── bot.log          # Execution and error logs generated during runtime
    └── downloads/           # Temporary storage for media files (images, audio) received via Telegram
```

## Setup

1.  **Obtain Telegram Bot Token:**
    -   Talk to `@BotFather` on Telegram to create a new bot and obtain the `TELEGRAM_BOT_TOKEN`.

2.  **Create a Gemini CLI Session (Get UUID):**
    -   Open a terminal and start a new session dedicated to Telegram.
        ```bash
        gemini -y -p "You are my Telegram assistant."
        ```
    -   List the sessions to copy the unique UUID of the newly created session.
        ```bash
        gemini --list-sessions
        ```

3.  **Configuration (.env):**
    -   Create a `.env` file inside the `src/` folder, or run the connector once to trigger the setup wizard.
    -   Fill in your token, chat ID, and the copied UUID:
    ```ini
    TELEGRAM_BOT_TOKEN=your_telegram_bot_token
    TELEGRAM_CHAT_ID=your_chat_id
    GEMINI_SESSION_UUID=your_gemini_session_uuid_here
    ```

4.  **Configure AI Personality & Guidelines (Optional):**
    -   Refer to the `gemini.md_sample` and `personality.md_sample` files provided in the `.gemini/` folder.
    -   Modify the contents to fit your needs, and rename them by removing the `_sample` extension (i.e., save as `gemini.md` and `personality.md`). The AI will strictly adhere to these customized rules.

## Installation & Run

You can either compile the source code yourself or download a pre-built executable to get started immediately.

### Option 1: Download from GitHub Releases (Recommended)
1. Go to the [Releases] page of this repository.
2. Download the appropriate executable for your operating system (e.g., `gemini-connector_windows_x64.exe`) and place it in the `golang/gemini-connector/bin/` directory or any desired location.

### Option 2: Build from Source
If you have Go installed, you can compile the source code directly.

```bash
cd golang/gemini-connector/src
go build -o ../bin/gemini-connector.exe
```

### Run
Simply run the compiled or downloaded executable as a standalone background process.

```bash
cd golang/gemini-connector/bin
./gemini-connector.exe
```
(Tip: Use `nohup` or background execution tools to keep the connector running continuously on a server without blocking the foreground shell.)

## ⚠️ Disclaimer and Risk Warning

This connector explicitly executes the **Gemini CLI in `YOLO (-y)` mode** to achieve full automation and convenience. This means that all tool invocations and local file system controls (modifications, deletions, etc.) determined by the AI will be executed immediately **without requiring any prior user confirmation**.

*   The developers and contributors of this project assume **absolutely no liability** for any direct or indirect damages, data loss, unauthorized system file modifications, or security vulnerabilities that may arise from the AI's hallucinations or incorrect judgments.
*   **All risks and responsibilities associated with executing this code and utilizing its outputs rest entirely with the user.** It is strongly recommended to run this connector within a secure sandbox environment or with restricted permissions.