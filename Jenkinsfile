pipeline {
    agent any

    options {
        timestamps()
        disableConcurrentBuilds()
        // Generous headroom: testcontainers pulls postgres:16-alpine + the Ryuk
        // reaper image on first run, on top of starting containers per test.
        timeout(time: 20, unit: 'MINUTES')
    }

    // All blank by default — docker-compose.yml's ${VAR:-default} only falls
    // back on an empty value, so an untouched build keeps compose's own
    // defaults. Fill one in via "Build with Parameters" to override just that
    // value for this run.
    parameters {
        string(name: 'SYNC_PORT', defaultValue: '', description: 'Host+container port for sync (compose default: 50052)')
        string(name: 'DATABASE_URL', defaultValue: '', description: 'Postgres DSN (compose default: host.docker.internal:15432)')
        string(name: 'CORE_URL', defaultValue: '', description: 'Core base URL (compose default: host.docker.internal:9999)')
        string(name: 'PLUGIN_BASE_URL', defaultValue: '', description: 'URL Core uses to reach this plugin (compose default: derived from SYNC_PORT)')
        string(name: 'SYNC_SHARED_COLLECTIONS', defaultValue: '', description: 'Comma-separated tenant-wide collections (compose default: none)')
        string(name: 'SYNC_TOMBSTONE_RETENTION', defaultValue: '', description: 'Go duration (compose default: 2160h)')
        password(name: 'PLUGIN_API_KEY', defaultValue: '', description: 'Shared secret with Core (compose default: change-me-plugin-key)')
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
            //
            // Every param above passes through as an env var; docker-compose.yml's
            // ${VAR:-default} falls back to its own default when the param was
            // left blank, so an untouched build behaves exactly as before.
            environment {
                SYNC_PORT = "${params.SYNC_PORT}"
                DATABASE_URL = "${params.DATABASE_URL}"
                CORE_URL = "${params.CORE_URL}"
                PLUGIN_BASE_URL = "${params.PLUGIN_BASE_URL}"
                SYNC_SHARED_COLLECTIONS = "${params.SYNC_SHARED_COLLECTIONS}"
                SYNC_TOMBSTONE_RETENTION = "${params.SYNC_TOMBSTONE_RETENTION}"
                PLUGIN_API_KEY = "${params.PLUGIN_API_KEY}"
            }
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
