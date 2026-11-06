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
