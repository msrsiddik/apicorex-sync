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
        GOFLAGS   = '-mod=mod'
        IMAGE_TAG = "apicorex-sync:${env.BUILD_NUMBER}"
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

        stage('Docker Image') {
            steps {
                sh "docker build -t ${IMAGE_TAG} -t apicorex-sync:latest ."
            }
        }
    }

    post {
        always {
            cleanWs()
        }
    }
}
