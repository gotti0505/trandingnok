#!/usr/bin/env bash
# Run this script AFTER installing Go (https://go.dev/dl/)
# Usage: bash scripts/install-deps.sh

set -e
cd "$(dirname "$0")/.."

echo "==> go mod tidy (resolves go.sum)"
go mod tidy

echo "==> Fetching all declared dependencies..."
go get github.com/gin-gonic/gin@latest
go get gorm.io/gorm@latest
go get gorm.io/driver/postgres@latest
go get github.com/redis/go-redis/v9@latest
go get github.com/golang-jwt/jwt/v5@latest
go get github.com/robfig/cron/v3@latest
go get go.uber.org/zap@latest
go get gopkg.in/yaml.v3@latest
go get github.com/gorilla/websocket@latest
go get github.com/stretchr/testify@latest

echo "==> go mod tidy (cleanup)"
go mod tidy

echo "==> Verification"
go list ./...
echo "Done. Run 'go test ./...' to verify."
