pipeline {
    agent any

    options {
        timestamps()
        disableConcurrentBuilds()
        // Generous headroom: testcontainers pulls postgres:16-alpine + the Ryuk
        // reaper image on first run, on top of starting containers per test.
        timeout(time: 20, unit: 'MINUTES')
    }

    environment {
        GOFLAGS = '-mod=mod'
    }

    stages {
        stage('Vet') {
            steps {
                sh 'go vet ./...'
            }
        }

        stage('Test') {
            // Integration tests (internal/syncdb) use testcontainers-go: they spin
            // up a real Postgres 16 container via the Docker socket and skip
            // automatically only if no container engine is reachable. This agent
            // has Docker, so they run for real (see TESTING.md).
            steps {
                sh 'go test ./... -v -timeout 5m'
            }
        }

        stage('Build') {
            steps {
                sh 'CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o out/sync ./cmd/sync'
            }
        }

        stage('Deploy') {
            // Builds the image and (re)starts the container via compose. Needs
            // the shared Postgres (apicorex repo's compose) and Core already
            // running — compose only depends_on services defined in this file.
            steps {
                sh 'docker compose up -d --build'
            }
        }
    }

    post {
        always {
            cleanWs()
        }
    }
}
