const { defineConfig } = require('cypress');
const fs = require('fs');
const path = require('path');
const axios = require('axios');

module.exports = defineConfig({
    e2e: {
        setupNodeEvents(on, config) {
            // Load environment settings from command line or environment variables
            const testEnv = process.env.TEST_ENV || 'dev';
            console.log(`Running tests against environment: ${testEnv}`);

            // Implementation of custom tasks
            on('task', {
                // Reset test state in all services
                resetTestState: async () => {
                    try {
                        // TODO This is a placeholder. Need to implement
                        // specific cleanup logic for services
                        console.log(`Resetting test state...`);
                        return true;
                    } catch (error) {
                        console.error('Error resetting test state:', error);
                        return false;
                    }
                },

                // Task to simulate service restart (used in writer.cy.js)
                simulateServiceRestart: async ({ service }) => {
                    // In a real environment, this might connect to k8s API 
                    // or call a specific endpoint to trigger a restart
                    console.log(`Simulating restart of ${service} service`);

                    // TODO Mock implementation - in real use implement actual restart logic
                    try {
                        // TODO Placeholder - call a restart API
                        await new Promise(resolve => setTimeout(resolve, 1000));
                        console.log(`${service} service restarted`);
                        return true;
                    } catch (error) {
                        console.error(`Error restarting ${service} service:`, error);
                        return false;
                    }
                },

                // Read a log file (useful for checking service logs during tests)
                readLogFile: (filePath) => {
                    if (fs.existsSync(filePath)) {
                        return fs.readFileSync(filePath, 'utf8');
                    }
                    return null;
                },

                // Query OpenTelemetry collector for traces
                queryTraces: async ({ traceId }) => {
                    try {
                        // TODO This is a placeholder. Need to implement this to query 
                        // actual tracing backend (Jaeger)
                        console.log(`Querying for trace ID: ${traceId}`);
                        // Mock response
                        return {
                            found: true,
                            spans: [
                                { name: 'enqueuer-process', status: 'ok' },
                                { name: 'nats-publish', status: 'ok' },
                                { name: 'memorizer-process', status: 'ok' },
                                { name: 'redis-store', status: 'ok' },
                                { name: 'writer-process', status: 'ok' },
                            ]
                        };
                    } catch (error) {
                        console.error('Error querying traces:', error);
                        return { found: false, error: error.message };
                    }
                }
            });

            // Load environment variables based on the selected environment
            const envConfig = require(`${config.fixturesFolder}/environments.json`);
            config.env = {
                ...config.env,
                ...envConfig[testEnv],
                ENVIRONMENT: testEnv
            };

            // Add environment name to the title
            config.env.titleSuffix = ` (${testEnv.toUpperCase()})`;

            return config;
        },
        baseUrl: 'https://dev.enqueuer.fullstack.pw', // Default baseUrl, will be overridden by environment config
        specPattern: 'cypress/e2e/**/*.cy.{js,jsx,ts,tsx}',
        supportFile: 'cypress/support/e2e.js',
        screenshotsFolder: 'cypress/screenshots',
        videosFolder: 'cypress/videos',
        viewportWidth: 1280,
        viewportHeight: 720,
        defaultCommandTimeout: 10000,
        requestTimeout: 8000,
        responseTimeout: 30000,
        retries: {
            runMode: 2,      // Retry failing tests in CI
            openMode: 0      // Don't retry in GUI mode
        },
    },
    reporter: 'junit',
    reporterOptions: {
        mochaFile: 'cypress/results/results-[hash].xml',
        toConsole: true,
    },
    env: {
        // Default values, will be overridden by environment-specific config
        ENQUEUER_URL: 'https://dev.enqueuer.fullstack.pw',
        MEMORIZER_URL: 'https://dev.memorizer.fullstack.pw',
        WRITER_URL: 'https://dev.writer.fullstack.pw',
    },
    experimentalStudio: false,  // Enable/disable Cypress Studio
    video: true,
    videosFolder: 'cypress/videos',
    videoCompression: 32,
    screenshotOnRunFailure: true,
    trashAssetsBeforeRuns: true,
});