# Ticketon Checker Makefile
.PHONY: help build run clean install dev test logs stop restart status deps check-env

# Variables
BINARY_NAME=ticketon-checker
BINARY_PATH=./bin/$(BINARY_NAME)
MAIN_FILE=ticketon.go
PID_FILE=ticketon.pid
LOG_FILE=checker.log

# Go parameters
GOCMD=go
GOBUILD=$(GOCMD) build
GOCLEAN=$(GOCMD) clean
GOTEST=$(GOCMD) test
GOGET=$(GOCMD) get
GOMOD=$(GOCMD) mod
GOFMT=$(GOCMD) fmt

# Build flags
BUILD_FLAGS=-ldflags="-s -w"
DEV_FLAGS=-race

# Default target
help: ## Show this help message
	@echo "Ticketon Event Checker - Available commands:"
	@echo ""
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_-]+:.*##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)
	@echo ""

# Development commands
dev: check-env ## Run in development mode with hot reload
	@echo "Starting in development mode..."
	@$(GOCMD) run $(DEV_FLAGS) $(MAIN_FILE)

run: check-env ## Run the application directly
	@echo "Running $(BINARY_NAME)..."
	@$(GOCMD) run $(MAIN_FILE)

# Build commands
build: deps ## Build the binary
	@echo "Building $(BINARY_NAME)..."
	@mkdir -p bin
	@$(GOBUILD) $(BUILD_FLAGS) -o $(BINARY_PATH) $(MAIN_FILE)
	@echo "Binary built: $(BINARY_PATH)"

build-linux: deps ## Build for Linux
	@echo "Building for Linux..."
	@mkdir -p bin
	@GOOS=linux GOARCH=amd64 $(GOBUILD) $(BUILD_FLAGS) -o $(BINARY_PATH)-linux $(MAIN_FILE)
	@echo "Linux binary built: $(BINARY_PATH)-linux"

build-windows: deps ## Build for Windows
	@echo "Building for Windows..."
	@mkdir -p bin
	@GOOS=windows GOARCH=amd64 $(GOBUILD) $(BUILD_FLAGS) -o $(BINARY_PATH).exe $(MAIN_FILE)
	@echo "Windows binary built: $(BINARY_PATH).exe"

build-mac: deps ## Build for macOS
	@echo "Building for macOS..."
	@mkdir -p bin
	@GOOS=darwin GOARCH=amd64 $(GOBUILD) $(BUILD_FLAGS) -o $(BINARY_PATH)-darwin $(MAIN_FILE)
	@echo "macOS binary built: $(BINARY_PATH)-darwin"

build-all: build-linux build-windows build-mac ## Build for all platforms
	@echo "All binaries built successfully!"

# Service management
start: build check-env ## Build and start the service in background
	@if [ -f $(PID_FILE) ]; then \
		echo "Service already running (PID: $$(cat $(PID_FILE)))"; \
		exit 1; \
	fi
	@echo "Starting $(BINARY_NAME) service..."
	@nohup $(BINARY_PATH) > $(LOG_FILE) 2>&1 & echo $$! > $(PID_FILE)
	@echo "Service started with PID: $$(cat $(PID_FILE))"
	@echo "Logs: $(LOG_FILE)"

stop: ## Stop the running service
	@if [ ! -f $(PID_FILE) ]; then \
		echo "No PID file found. Service not running?"; \
		exit 1; \
	fi
	@PID=$$(cat $(PID_FILE)); \
	echo "Stopping service (PID: $$PID)..."; \
	kill $$PID && rm -f $(PID_FILE) && echo "Service stopped successfully" || echo "Failed to stop service"

restart: stop start ## Restart the service

status: ## Check service status
	@if [ -f $(PID_FILE) ]; then \
		PID=$$(cat $(PID_FILE)); \
		if ps -p $$PID > /dev/null; then \
			echo "Service is running (PID: $$PID)"; \
		else \
			echo "Service is not running (stale PID file)"; \
			rm -f $(PID_FILE); \
		fi; \
	else \
		echo "Service is not running"; \
	fi

# Dependency management
deps: ## Download dependencies
	@echo "Downloading dependencies..."
	@$(GOMOD) download
	@$(GOMOD) tidy

update-deps: ## Update dependencies
	@echo "Updating dependencies..."
	@$(GOGET) -u all
	@$(GOMOD) tidy

# Development tools
fmt: ## Format Go code
	@echo "Formatting code..."
	@$(GOFMT) ./...

vet: ## Run go vet
	@echo "Running go vet..."
	@$(GOCMD) vet ./...

test: ## Run tests
	@echo "Running tests..."
	@$(GOTEST) -v ./...

# Environment and validation
check-env: ## Check environment configuration
	@echo "Checking environment configuration..."
	@if [ ! -f .env ]; then \
		echo "ERROR: .env file not found!"; \
		echo "Create .env file with:"; \
		echo "  TELEGRAM_BOT_TOKEN=your_bot_token"; \
		echo "  ADMIN_CHAT_ID=your_chat_id"; \
		exit 1; \
	fi
	@if ! grep -q "TELEGRAM_BOT_TOKEN=" .env || ! grep -q "ADMIN_CHAT_ID=" .env; then \
		echo "ERROR: .env file missing required variables!"; \
		echo "Required variables: TELEGRAM_BOT_TOKEN, ADMIN_CHAT_ID"; \
		exit 1; \
	fi
	@echo "Environment configuration OK ✓"

setup: ## Initial setup (create .env template if not exists)
	@if [ ! -f .env ]; then \
		echo "Creating .env template..."; \
		echo "TELEGRAM_BOT_TOKEN=your_telegram_bot_token_here" > .env; \
		echo "ADMIN_CHAT_ID=your_admin_chat_id_here" >> .env; \
		echo ".env template created. Please edit it with your actual values."; \
	else \
		echo ".env file already exists"; \
	fi
	@if [ ! -f go.mod ]; then \
		echo "Initializing Go module..."; \
		$(GOCMD) mod init ticketon-checker; \
	fi
	@$(MAKE) deps

# Logging
logs: ## Show recent logs
	@if [ -f $(LOG_FILE) ]; then \
		tail -f $(LOG_FILE); \
	else \
		echo "No log file found: $(LOG_FILE)"; \
	fi

logs-tail: ## Tail logs in real-time
	@tail -f $(LOG_FILE) 2>/dev/null || echo "No log file found: $(LOG_FILE)"

logs-clear: ## Clear logs
	@echo "Clearing logs..."
	@> $(LOG_FILE) && echo "Logs cleared" || echo "No log file to clear"

# Cleanup
clean: ## Clean build artifacts and logs
	@echo "Cleaning up..."
	@$(GOCLEAN)
	@rm -rf bin/
	@rm -f $(PID_FILE)
	@echo "Cleanup complete"

clean-all: clean ## Clean everything including logs and data files
	@echo "Cleaning all data..."
	@rm -f $(LOG_FILE)
	@rm -f seen_events.json
	@rm -f subscribers.txt
	@echo "All data cleaned"

# Quick start
quick-start: setup build start ## Quick setup and start

# Information
info: ## Show project information
	@echo "Ticketon Event Checker"
	@echo "======================"
	@echo "Binary: $(BINARY_PATH)"
	@echo "Main file: $(MAIN_FILE)"
	@echo "Log file: $(LOG_FILE)"
	@echo "PID file: $(PID_FILE)"
	@echo ""
	@echo "Go version: $$(go version)"
	@echo "Project path: $$(pwd)"