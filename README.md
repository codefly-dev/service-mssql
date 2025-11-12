## Build and push multi-platform images

### Prerequisites
1. Make sure you're logged into Docker Hub with access to the `codeflydev` organization:
   ```bash
   docker login
   ```

2. Set up buildx if you haven't already:
   ```bash
   docker buildx create --use
   ```

### Build and push the multi-arch image for Microsoft SQL Server

```bash 
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  -t codeflydev/mssql-alembic:latest \
  -f migrations/Dockerfile.alembic \
  --push \
  migrations/
```

**Note:** Make sure you have push access to the `codeflydev` Docker Hub organization before running the push command.