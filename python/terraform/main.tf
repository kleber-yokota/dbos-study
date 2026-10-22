# "AWS" infrastructure of the diagram, emulated by Floci (port 4566).
#
#   EC2    -> aws_instance (client: FastAPI app)
#   RDS    -> aws_db_instance (Postgres 18.1) = DBOS system database
#   LAMBDA -> aws_lambda_function (dbos-worker) = workers

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

# Emulated RDS — the Postgres 18.1 that is the DBOS system database
resource "aws_db_instance" "dbos" {
  identifier        = "dbos-db"
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

# Emulated Lambda — the DBOS workers
resource "aws_lambda_function" "worker" {
  function_name = "dbos-worker"
  handler       = "worker.handler"
  runtime       = "python3.13"
  role          = var.floci_role_arn
  filename      = "worker.zip"
  timeout       = 90
  memory_size   = 128
  depends_on    = [aws_db_instance.dbos]

  environment {
    variables = {
      DBOS_SYSTEM_DATABASE_URL = "postgresql://dbos:${var.db_password}@${aws_db_instance.dbos.endpoint}/dbos"
    }
  }
}

# Emulated EC2 — the diagram node that runs the client (FastAPI app).
# Floci spins up a container and runs the UserData on boot,
# exactly like real AWS.
resource "aws_instance" "client" {
  # Floci catalog: Amazon Linux 2023 (UserData installs python 3.11 via dnf —
  # the base ships 3.9, below dbos' >=3.10 minimum)
  ami           = "ami-amazonlinux2023"
  instance_type = "t3.micro"
  tags          = { Name = "dbos-client" }
  depends_on    = [aws_lambda_function.worker]

  user_data = <<-EOT
    #!/bin/bash
    set -e
    exec > /var/log/user-data.log 2>&1
    export DBOS_SYSTEM_DATABASE_URL="postgresql://dbos:${var.db_password}@${aws_db_instance.dbos.endpoint}/dbos"
    export FLOCI_ENDPOINT="$${AWS_ENDPOINT_URL:-http://localhost:4566}"
    export AWS_ACCESS_KEY_ID=floci
    export AWS_SECRET_ACCESS_KEY=floci
    export AWS_DEFAULT_REGION=us-east-1
    # AL2023 ships Python 3.9; dbos needs >= 3.10, so install 3.11 + pip.
    dnf install -y -q python3.11 python3.11-pip
    # Versions pinned to pyproject.toml / the study run (reproducible boot)
    python3.11 -m pip install --quiet 'dbos~=3.3' 'boto3~=1.43' 'fastapi~=0.159' 'uvicorn~=0.54'
    mkdir -p /root/api
    echo '${filebase64("${path.root}/../local_aws.py")}' | base64 -d > /root/api/local_aws.py
    echo '${filebase64("${path.root}/../app.py")}' | base64 -d > /root/api/app.py
    cd /root/api
    nohup python3.11 -m uvicorn app:app --host 0.0.0.0 --port 8000 > /var/log/api.log 2>&1 &
    sleep 3
    echo "[api] smoke dispatch: $(curl -s 'http://127.0.0.1:8000/workflows?name=process_task')"
  EOT
}

output "system_database_url" {
  value     = "postgresql://dbos:${var.db_password}@${aws_db_instance.dbos.endpoint}/dbos"
  sensitive = true
}
