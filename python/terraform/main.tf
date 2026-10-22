# "AWS" infrastructure of the diagram, emulated by Floci (port 4566).
#
#   EC2    -> aws_instance (client: enqueue.py)
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

variable "task_count" {
  description = "How many tasks the EC2 node enqueues on startup"
  type        = number
  default     = 5
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

# Emulated EC2 — the diagram node that runs the client (enqueue.py).
# Floci spins up a container and runs the UserData on boot,
# exactly like real AWS.
resource "aws_instance" "client" {
  # Floci catalog: Ubuntu 24.04 x86_64 (python 3.12 — dbos requires >=3.10)
  ami           = "ami-ubuntu2404-amd64"
  instance_type = "t3.micro"
  tags          = { Name = "dbos-client" }
  depends_on    = [aws_lambda_function.worker]

  user_data = <<-EOT
    #!/bin/bash
    set -e
    export DBOS_SYSTEM_DATABASE_URL="postgresql://dbos:${var.db_password}@${aws_db_instance.dbos.endpoint}/dbos"
    export FLOCI_ENDPOINT="$${AWS_ENDPOINT_URL:-http://localhost:4566}"
    export AWS_ACCESS_KEY_ID=floci
    export AWS_SECRET_ACCESS_KEY=floci
    export AWS_DEFAULT_REGION=us-east-1
    if ! python3 -m pip --version 2>/dev/null; then
      curl -sSf https://bootstrap.pypa.io/get-pip.py -o /tmp/get-pip.py
      python3 /tmp/get-pip.py --break-system-packages
    fi
    python3 -m pip install --quiet --break-system-packages dbos boto3 fastapi uvicorn
    mkdir -p /root/api
    echo '${filebase64("${path.root}/../local_aws.py")}' | base64 -d > /root/api/local_aws.py
    echo '${filebase64("${path.root}/../app.py")}' | base64 -d > /root/api/app.py
    cd /root/api
    nohup python3 -m uvicorn app:app --host 0.0.0.0 --port 8000 > /var/log/api.log 2>&1 &
    sleep 3
    echo "[api] smoke dispatch: $(curl -s 'http://127.0.0.1:8000/workflows?name=process_task')"
  EOT
}

output "system_database_url" {
  value     = "postgresql://dbos:${var.db_password}@${aws_db_instance.dbos.endpoint}/dbos"
  sensitive = true
}
