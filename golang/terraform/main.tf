# Go "AWS" stack — independent of the Python stack (own RDS, Lambda and EC2).
#
#   EC2    -> aws_instance (Go API on :8000)
#   RDS    -> aws_db_instance (Postgres 18.1) = DBOS system database
#   LAMBDA -> aws_lambda_function (dbos-worker-go, static Go binary in bootstrap)

terraform {
  required_providers {
    aws = {
      source = "hashicorp/aws"
    }
  }
}

provider "aws" {
  region                      = "us-east-1"
  access_key                  = "floci"
  secret_key                  = "floci"
  s3_use_path_style           = true
  skip_credentials_validation = true
  skip_metadata_api_check     = true
  skip_requesting_account_id  = true
  endpoints {
    rds    = "http://localhost:4566"
    ec2    = "http://localhost:4566"
    lambda = "http://localhost:4566"
    iam    = "http://localhost:4566"
    sts    = "http://localhost:4566"
    s3     = "http://localhost:4566"
  }
}

variable "db_password" {
  type      = string
  default   = "dbos_local"
  sensitive = true
}

variable "floci_role_arn" {
  description = "ARN of the Lambda execution role (accepted by Floci without a real role)"
  type        = string
  default     = "arn:aws:iam::000000000000:role/lambda-role"
}

# Emulated RDS — the Go stack system database (isolated from the Python dbos-db)
resource "aws_db_instance" "dbos_go" {
  identifier        = "dbos-go-db"
  engine            = "postgres"
  engine_version    = "18.1"
  db_name           = "dbos"
  username          = "dbos"
  password          = var.db_password
  instance_class    = "db.t3.micro"
  allocated_storage = 5
  skip_final_snapshot = true
  deletion_protection = false
}

# Emulated Lambda — Go worker (static binary as bootstrap, provided.al2023)
resource "aws_lambda_function" "worker_go" {
  function_name = "dbos-worker-go"
  handler       = "bootstrap"
  runtime       = "provided.al2023"
  role          = var.floci_role_arn
  filename      = "bootstrap.zip"
  timeout       = 90
  memory_size   = 128
  depends_on    = [aws_db_instance.dbos_go]

  environment {
    variables = {
      DBOS_SYSTEM_DATABASE_URL = "postgresql://dbos:${var.db_password}@${aws_db_instance.dbos_go.endpoint}/dbos"
    }
  }
}

# Emulated EC2 — the Go API node. The UserData compiles the binary inside the
# container (official Go toolchain, since the DBOS SDK requires >= 1.25).
resource "aws_instance" "client_go" {
  ami           = "ami-amazonlinux2023"
  instance_type = "t3.micro"
  tags          = { Name = "dbos-client-go" }
  depends_on    = [aws_lambda_function.worker_go]

  user_data = <<-EOT
    #!/bin/bash
    set -e
    exec > /var/log/user-data.log 2>&1
    export DBOS_SYSTEM_DATABASE_URL="postgresql://dbos:${var.db_password}@${aws_db_instance.dbos_go.endpoint}/dbos"
    export FLOCI_ENDPOINT="$${AWS_ENDPOINT_URL:-http://localhost:4566}"
    export AWS_ACCESS_KEY_ID=floci
    export AWS_SECRET_ACCESS_KEY=floci
    export AWS_DEFAULT_REGION=us-east-1
    export AWS_REGION=us-east-1

    # The AL2023 base image is minimal (no tar, no Go)
    dnf install -y -q tar
    # Go toolchain (AL2023 ships no Go; the DBOS SDK requires >= 1.25)
    GOVER=go1.27.3
    curl -sSL "https://go.dev/dl/$GOVER.linux-amd64.tar.gz" -o /tmp/go.tar.gz
    tar -C /usr/local -xzf /tmp/go.tar.gz
    export PATH=/usr/local/go/bin:$PATH

    mkdir -p /root/api/src
    echo '${filebase64("${path.root}/../go.mod")}' | base64 -d > /root/api/src/go.mod
    echo '${filebase64("${path.root}/../api/main.go")}' | base64 -d > /root/api/src/main.go
    cd /root/api/src
    # go.sum is regenerated here (user_data has a 16KB limit on AWS)
    go mod tidy
    go build -o /root/api/api .

    nohup /root/api/api > /var/log/api.log 2>&1 &
    sleep 3
    echo "[api-go] smoke dispatch: $(curl -s 'http://127.0.0.1:8000/workflows?name=process_task')"
  EOT
}

output "system_database_url" {
  value     = "postgresql://dbos:${var.db_password}@${aws_db_instance.dbos_go.endpoint}/dbos"
  sensitive = true
}
