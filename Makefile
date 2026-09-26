ORANGE := \033[38;5;208m
GREEN  := \033[32m
RESET  := \033[0m

.PHONY: build run

build:
	@printf "$(ORANGE)building orbit...$(RESET)\n"
	@go build -o bin/orbit

run: build
	@printf "$(GREEN)running orbit...$(RESET)\n"
	@./bin/orbit
