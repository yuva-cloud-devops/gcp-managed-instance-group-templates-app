# GCP MIG Test Application (`gcp-mig-test-app`)

A production-grade, zero-dependency Go HTTP JSON API service designed to validate **Google Cloud Managed Instance Groups (MIGs)**, **Instance Templates**, **Packer Golden Images**, **Auto-Healing**, **Rolling Updates**, and **Global External Application Load Balancers**.

---

## Architecture & CI/CD Workflow

```text
GitHub Push / Merge
  │
  ▼
Cloud Build (cloudbuild.yaml)
  ├── 1. Build Static Linux Go Binary (CGO_ENABLED=0, GOOS=linux)
  ├── 2. Initialize Packer (googlecompute plugin)
  ├── 3. Bake Golden Image on Debian 12 (packer/mig-test-app.pkr.hcl)
  └── 4. Export Image Metadata JSON & Upload to GCS Bucket
        │
        ▼
Terraform Infrastructure Pipeline
  ├── Read Image Metadata JSON from GCS (or image family: mig-test-app)
  ├── Create Google Compute Instance Template
  └── Deploy Regional Managed Instance Group (Regional MIG)
        │
        ├── Auto-Healing (/health probe)
        ├── Zero-Downtime Rolling Updates (PROACTIVE / REPLACE)
        └── Global External Application Load Balancer
```

---

## Project Structure

```text
gcp-managed-instance-group-templates-app/
├── main.go                     # Pure Go HTTP JSON API with GCE metadata retrieval
├── go.mod                      # Go standard library only (no external dependencies)
├── cloudbuild.yaml             # CI/CD pipeline (Go build + Packer + GCS export)
├── README.md                   # Full documentation & usage guide
├── systemd/
│   └── mig-test-app.service    # Linux systemd service unit
└── packer/
    └── mig-test-app.pkr.hcl    # Minimalist Packer HCL2 Golden Image template
```

---

## Configuration (Environment Variables)

The application configures itself via environment variables with production defaults:

| Variable | Default | Description |
| :--- | :--- | :--- |
| `PORT` | `3000` | TCP port on which the HTTP server listens |
| `VERSION` | `v1.0.0` | Application release version |
| `ENVIRONMENT` | `dev` | Target environment (`dev`, `staging`, `prod`) |
| `BUILD_ID` | `local` | Build or image identifier (e.g. `packer-image-v1`, git commit SHA) |

---

## API Endpoints Reference

All endpoints return structured JSON responses.

### 1. `GET /` (or `GET /api`)
Returns complete VM identity, location, and deployment metadata:

```bash
curl http://localhost:3000/
```

```json
{
  "status": "success",
  "message": "Request successfully served by GCP MIG",
  "hostname": "web-mumbai-abc1",
  "instance": {
    "name": "web-mumbai-abc1",
    "id": "1234567890123456789",
    "region": "asia-south1",
    "zone": "asia-south1-a",
    "machine_type": "e2-medium",
    "internal_ip": "10.10.1.5",
    "external_ip": "Unavailable",
    "tags": [
      "web-backend",
      "http-server"
    ]
  },
  "service_account": "sa-mig-runtime@project-id.iam.gserviceaccount.com",
  "version": "v1.0.0",
  "environment": "prod",
  "build_id": "packer-image-v1",
  "timestamp": "2026-11-24T09:45:00Z"
}
```

> **Note**: Outside Google Compute Engine (e.g. running locally), metadata queries safely fall back to `"Unavailable"` without crashing.

### 2. `GET /health`
MIG Auto-Healing and Load Balancer health check probe (HTTP `200 OK`):

```bash
curl http://localhost:3000/health
```

```json
{
  "status": "healthy"
}
```

### 3. `GET /ready`
Readiness probe for traffic admission (HTTP `200 OK`):

```bash
curl http://localhost:3000/ready
```

```json
{
  "status": "ready"
}
```

### 4. `GET /info`
Application provenance and Go runtime version:

```bash
curl http://localhost:3000/info
```

```json
{
  "app": "gcp-mig-test-app",
  "version": "v1.0.0",
  "environment": "prod",
  "build_id": "packer-image-v1",
  "go_version": "go1.27.3"
}
```

---

## Local Development & Build

### Run Locally

```bash
go run main.go
```
The server will start listening on [http://localhost:3000](http://localhost:3000).

Test with curl:
```bash
curl http://localhost:3000/
curl http://localhost:3000/health
curl http://localhost:3000/ready
curl http://localhost:3000/info
```

### Build Binary Locally

```bash
go build -o mig-test-app .
./mig-test-app
```

### Compile Static Linux Binary (for GCE / Packer)

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o build/mig-test-app .
```

---

## Systemd Service

The application runs as a systemd-managed service on GCE VMs:

[`systemd/mig-test-app.service`](systemd/mig-test-app.service):
```ini
[Unit]
Description=GCP MIG Test Application
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/mig-test-app
Restart=always
RestartSec=5

Environment=PORT=3000
Environment=VERSION=v1.0.0
Environment=ENVIRONMENT=prod
Environment=BUILD_ID=packer-image-v1

User=nobody

[Install]
WantedBy=multi-user.target
```

---

## Packer Golden Image Template

[`packer/mig-test-app.pkr.hcl`](packer/mig-test-app.pkr.hcl) bakes a Debian 12 base image into a production-ready Golden Image:

```hcl
packer {
  required_plugins {
    googlecompute = {
      source  = "github.com/hashicorp/googlecompute"
      version = ">= 1.1.0"
    }
  }
}

variable "project_id" {
  type        = string
  description = "GCP Project ID"
}

variable "zone" {
  type        = string
  default     = "asia-south1-a"
  description = "GCP Zone for the builder VM"
}

source "googlecompute" "mig_app" {
  project_id              = var.project_id
  zone                    = var.zone
  source_image_family     = "debian-12"
  source_image_project_id = ["debian-cloud"]
  machine_type            = "e2-medium"
  disk_size               = 10
  ssh_username            = "packer"

  image_name        = "mig-test-app-{{timestamp}}"
  image_family      = "mig-test-app"
  image_description = "GCP MIG Test App Golden Image"
}

build {
  sources = ["source.googlecompute.mig_app"]

  # 1. Copy Go binary to VM
  provisioner "file" {
    source      = "build/mig-test-app"
    destination = "/tmp/mig-test-app"
  }

  # 2. Copy systemd service file to VM
  provisioner "file" {
    source      = "systemd/mig-test-app.service"
    destination = "/tmp/mig-test-app.service"
  }

  # 3. Install binary and enable systemd service
  provisioner "shell" {
    inline = [
      "sudo mv /tmp/mig-test-app /usr/local/bin/mig-test-app",
      "sudo chmod +x /usr/local/bin/mig-test-app",
      "sudo mv /tmp/mig-test-app.service /etc/systemd/system/mig-test-app.service",
      "sudo systemctl daemon-reload",
      "sudo systemctl enable mig-test-app.service"
    ]
  }
}
```

---

## Cloud Build CI/CD Pipeline

[`cloudbuild.yaml`](cloudbuild.yaml) automates compilation, image creation, and metadata export:

```yaml
steps:
  # 1. Build static Linux Go binary
  - id: "build-go-binary"
    name: "golang:latest"
    env:
      - "CGO_ENABLED=0"
      - "GOOS=linux"
      - "GOARCH=amd64"
    script: |
      mkdir -p build
      go build -o build/mig-test-app .

  # 2. Initialize Packer plugin
  - id: "packer-init"
    name: "hashicorp/packer:latest"
    args:
      - "init"
      - "packer/mig-test-app.pkr.hcl"

  # 3. Build Golden Image with Packer
  - id: "packer-build"
    name: "hashicorp/packer:latest"
    args:
      - "build"
      - "-var"
      - "project_id=${PROJECT_ID}"
      - "-var"
      - "zone=${_ZONE}"
      - "packer/mig-test-app.pkr.hcl"

  # 4. Get the created image JSON metadata and store in GCS
  - id: "export-image-metadata-to-gcs"
    name: "gcr.io/google.com/cloudsdktool/cloud-sdk:slim"
    env:
      - "PROJECT_ID=$PROJECT_ID"
      - "SHORT_SHA=$SHORT_SHA"
      - "METADATA_BUCKET=$_METADATA_BUCKET"
    script: |
      #!/usr/bin/env bash
      set -eo pipefail

      PROJ="${PROJECT_ID:-$(gcloud config get-value project 2>/dev/null)}"
      COMMIT_SHA="${SHORT_SHA:-latest}"

      echo "==> Fetching latest Golden Image metadata for family mig-test-app in project: ${PROJ}..."
      gcloud compute images describe-from-family mig-test-app \
        --project="${PROJ}" \
        --format="json" > image-metadata.json

      IMAGE_NAME=$(gcloud compute images describe-from-family mig-test-app \
        --project="${PROJ}" \
        --format="value(name)")

      echo "==> Generated JSON metadata for image: ${IMAGE_NAME}"
      cat image-metadata.json

      DEST="gs://${METADATA_BUCKET}/image-metadata/${IMAGE_NAME}/${COMMIT_SHA}/image-metadata.json"
      LATEST_DEST="gs://${METADATA_BUCKET}/image-metadata/latest/image-metadata.json"

      echo "....................................................................................."
      echo "📦 Uploading image metadata json to: ${DEST}"
      gsutil cp image-metadata.json "${DEST}"
      echo "📦 Uploading latest image metadata json to: ${LATEST_DEST}"
      gsutil cp image-metadata.json "${LATEST_DEST}"
      echo "....................................................................................."

substitutions:
  _ZONE: "asia-south1-a"
  _METADATA_BUCKET: "gcp-tfplans-instancetemplate-strorage-bucket"

options:
  logging: CLOUD_LOGGING_ONLY

timeout: "1200s"
```

### Triggering Cloud Build Manually

```bash
gcloud builds submit --config=cloudbuild.yaml \
  --substitutions=_ZONE="asia-south1-a",_METADATA_BUCKET="<YOUR_GCS_BUCKET>" \
  .
```

---

## Terraform Integration Guide

### Health Check & Regional Managed Instance Group (MIG)

```hcl
# HTTP Health Check for Auto-Healing & Load Balancer (Port 3000)
resource "google_compute_health_check" "mig_health_check" {
  name = "mig-test-app-health-check"

  http_health_check {
    port         = 3000
    request_path = "/health"
  }

  check_interval_sec  = 5
  timeout_sec         = 2
  healthy_threshold   = 2
  unhealthy_threshold = 3
}

# Regional Managed Instance Group
resource "google_compute_region_instance_group_manager" "regional_mig" {
  name               = "mig-test-app-regional-mig"
  region             = "asia-south1"
  base_instance_name = "web-mumbai"

  version {
    instance_template = google_compute_instance_template.mig_template.id
  }

  target_size = 2

  named_port {
    name = "http"
    port = 3000
  }

  # Auto-Healing Policy
  auto_healing_policies {
    health_check      = google_compute_health_check.mig_health_check.id
    initial_delay_sec = 60
  }

  # Zero-Downtime Rolling Update Policy
  update_policy {
    type                  = "PROACTIVE"
    minimal_action        = "REPLACE"
    max_surge_fixed       = 2
    max_unavailable_fixed = 0
  }
}
```

---

## Validation Scenarios

Using this app, engineers can easily validate key GCP reliability and traffic management features:

1. **Load Balancing & Distribution**
   - Run `while true; do curl -s http://<LB_IP>:3000/ | jq .instance.name; sleep 1; done`.
   - Observe responses alternating across different backend VM instance names and zones.
2. **Packer Image Verification**
   - Check `build_id` and `version` in the JSON response to verify that the running VMs are using the expected Golden Image build.
3. **Auto-Healing Validation**
   - SSH into one VM and stop the service: `sudo systemctl stop mig-test-app`.
   - Observe the health check fail on `GET /health` (port 3000) and GCP automatically terminate and replace the unhealthy VM.
4. **Zero-Downtime Rolling Updates**
   - Push a new version (`v2.0.0`) through Cloud Build and trigger a MIG rolling update.
   - Run a polling loop with `curl` to witness traffic smoothly migrate to the new version without any dropped requests.
5. **Regional Failover**
   - Drain or disable backends in one region and watch the Global External Application Load Balancer immediately route traffic to the remaining healthy region.
