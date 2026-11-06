# GCP MIG Test Application (`gcp-mig-test-app`)

A zero-dependency, production-grade Go HTTP service built with the Go standard library to validate **Google Cloud Managed Instance Groups (MIGs)**, **Instance Templates**, **Packer Golden Images**, **Auto-Healing**, **Rolling Updates**, and **Global External Application Load Balancers**.

---

## Architecture & Expected CI/CD Workflow

```text
GitHub Repo B
  → Cloud Build
  → Build Go Binary
  → Packer Image Build
  → Golden Image
  → Instance Template
  → Managed Instance Group
  → Auto-Healing
  → Rolling Updates
  → Global External Application Load Balancer
```

---

## Project Structure

```text
mig-test-app/
├── main.go
├── go.mod
├── README.md
└── systemd/
    └── mig-test-app.service
```

---

## Configuration (Environment Variables)

| Variable      | Default   | Description                                              |
| :------------ | :-------- | :------------------------------------------------------- |
| `PORT`        | `8080`    | TCP port on which the HTTP server listens                |
| `VERSION`     | `v1.0.0`  | Application semantic version                             |
| `ENVIRONMENT` | `dev`     | Deployment environment (`dev`, `staging`, `prod`)        |
| `BUILD_ID`    | `local`   | Packer Golden Image / CI build identifier                |

---

## 1. Local Development

Build and run the binary locally (outside GCP, metadata fields safely fall back to `"Unavailable"`):

```bash
go build -o mig-test-app
./mig-test-app
```

Run with custom environment variables:

```bash
PORT=8080 VERSION=v1.0.0 ENVIRONMENT=dev BUILD_ID=local ./mig-test-app
```

---

## 2. Linux Build

Compile a static Linux AMD64 binary suitable for Google Compute Engine VMs and Packer image builds:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o mig-test-app
```

---

## 3. API Examples

### `GET /` — Instance & Deployment Metadata

```bash
curl http://localhost:8080/
```

Example response on a Google Compute Engine VM:

```json
{
  "status": "success",
  "message": "Request successfully served by GCP MIG",
  "hostname": "web-mumbai-abc1",
  "instance": {
    "name": "web-mumbai-abc1",
    "id": "123456789",
    "region": "asia-south1",
    "zone": "asia-south1-a",
    "machine_type": "e2-medium",
    "internal_ip": "10.10.1.5",
    "external_ip": "Unavailable",
    "tags": [
      "web-backend"
    ]
  },
  "service_account": "sa-mig-runtime@project.iam.gserviceaccount.com",
  "version": "v1.0.0",
  "environment": "prod",
  "build_id": "packer-image-v1",
  "timestamp": "2026-11-06T12:00:00Z"
}
```

### `GET /health` — Load Balancer & Auto-Healing Health Probe

```bash
curl http://localhost:8080/health
```

```json
{
  "status": "healthy"
}
```

### `GET /ready` — Instance Readiness Probe

```bash
curl http://localhost:8080/ready
```

```json
{
  "status": "ready"
}
```

### `GET /info` — Application & Runtime Build Information

```bash
curl http://localhost:8080/info
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

## 4. Packer Deployment

During a Packer image build (or VM provisioning), install the compiled binary and register the `systemd` unit:

```bash
sudo cp mig-test-app /usr/local/bin/
sudo chmod +x /usr/local/bin/mig-test-app

sudo cp systemd/mig-test-app.service /etc/systemd/system/

sudo systemctl daemon-reload
sudo systemctl enable mig-test-app
sudo systemctl start mig-test-app

sudo systemctl status mig-test-app
```

Note: If your Packer provisioner uploads `mig-test-app.service` directly to the working directory, copy it via:

```bash
sudo cp mig-test-app.service /etc/systemd/system/
```

---

## 5. GCP Usage & Validation Guide

This service exposes real-time VM metadata and build provenance on `GET /` and `GET /info` so engineers can validate every layer of their Google Cloud compute and load-balancing stack:

1. **Packer Image Deployments**
   - Verify that `build_id` (e.g., `packer-image-v1`) and `version` match the baked Golden Image baked by Packer and Cloud Build.
   - Confirm `systemd` automatically starts `mig-test-app` on boot without manual intervention.

2. **Instance Template Updates**
   - When creating a new Instance Template pointing to a new Golden Image or updated environment variables (`VERSION`, `ENVIRONMENT`, `BUILD_ID`), query `GET /` and `GET /info` to confirm the new template attributes and `machine_type` / `tags` / `service_account` are active.

3. **Managed Instance Groups (MIGs)**
   - Send repeated requests against the Load Balancer IP (`watch -n 1 curl -s http://<LB_IP>/`) and inspect `instance.name`, `instance.id`, `instance.zone`, and `instance.internal_ip` to confirm multiple MIG instances are active across zones.

4. **Auto-Healing**
   - Simulate an instance failure by stopping the service on a VM (`sudo systemctl stop mig-test-app`) or blocking port `8080`.
   - Observe the MIG health check failing on `GET /health`, triggering GCP Auto-Healing to recreate the unhealthy VM with a new `instance.id` and `timestamp`.

5. **Rolling Updates**
   - Trigger a MIG rolling replace/restart to roll out `VERSION=v2.0.0` and `BUILD_ID=packer-image-v2`.
   - Poll `GET /` continuously during the rollout to observe zero-downtime transitions as traffic shifts from `v1.0.0` instances to `v2.0.0` instances.

6. **Regional Failover**
   - In a multi-region backend service (e.g., `asia-south1` and `us-central1`), scale down or simulate unhealthy backends in the primary region and verify `instance.region` and `instance.zone` in the `GET /` response seamlessly fail over to the secondary region.

7. **Global Load Balancer Traffic Distribution**
   - Issue requests from clients in different geographic regions (or via Cloud Shell in multiple locations) and inspect `instance.region`, `instance.zone`, and `instance.name` to verify the Global External Application Load Balancer routes requests to the closest healthy regional MIG and distributes traffic across backend instances.

---

## Terraform Health Check Example

Use the `/health` endpoint for both Managed Instance Group auto-healing policies and Load Balancer backend health checks:

```hcl
http_health_check {
  port         = 8080
  request_path = "/health"
}
```

