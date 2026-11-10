# GCP MIG Test Application (`gcp-mig-test-app`)

A lightweight, zero-dependency Go HTTP service used to validate **Google Cloud Managed Instance Groups (MIGs)**, **Instance Templates**, **Packer Golden Images**, **Auto-Healing**, **Rolling Updates**, and **Global External Application Load Balancers**.

---

## Project Structure

```text
mig-test-app/
├── main.go                     # Go HTTP server with GCE metadata retrieval
├── go.mod                      # Go standard library only
├── cloudbuild.yaml             # CI pipeline to build Go binary & Packer image
├── README.md                   # Documentation
├── systemd/
│   └── mig-test-app.service    # systemd service unit
└── packer/
    └── mig-test-app.pkr.hcl    # Single, simple Packer HCL2 template
```

---

## 1. Local Development

```bash
go build -o mig-test-app
./mig-test-app
```

---

## 2. Linux Build

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o build/mig-test-app .
```

---

## 3. Endpoints & UI Dashboard

- `GET /` — 
  - **Browser access**: Renders an interactive, responsive **GCP MIG Visualizer Dashboard** displaying real-time VM identity cards, zone/region tags, live polling (auto-refresh) to observe load balancing across MIG VMs, and formatted JSON.
  - **API / curl access**: Returns the standard JSON response payload.
- `GET /api` or `GET /?format=json` — Direct raw JSON response.
- `GET /health` — Returns `{"status": "healthy"}` (HTTP 200).
- `GET /ready` — Returns `{"status": "ready"}` (HTTP 200).
- `GET /info` — Returns app name, version, environment, build ID, and Go version.

---

## 4. Packer Golden Image Build (via Cloud Build)

Submit the build directly to Google Cloud Build:

```bash
gcloud builds submit --config=cloudbuild.yaml --substitutions=_ZONE="asia-south1-a" .
```

This will:
1. Compile the Linux Go binary to `build/mig-test-app`.
2. Run Packer on `debian-12`.
3. Copy the binary and `systemd` unit to the VM.
4. Enable `mig-test-app.service` to start on boot.
5. Create a Golden Image in the `mig-test-app` image family.
6. Export the image JSON metadata and upload it to the GCS bucket (`gs://<PROJECT_ID>-image-metadata/image-metadata.json`).

---

## 5. Terraform Health Check Example

```hcl
http_health_check {
  port         = 8080
  request_path = "/health"
}
```
