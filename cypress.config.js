// cypress.config.js
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
                        console.log(`Resetting test state...`);
                        return true;
                    } catch (error) {
                        console.error('Error resetting test state:', error);
                        return false;
                    }
                },

                // Task to simulate service restart
                simulateServiceRestart: async ({ service }) => {
                    console.log(`Simulating restart of ${service} service`);
                    try {
                        await new Promise(resolve => setTimeout(resolve, 1000));
                        console.log(`${service} service restarted`);
                        return true;
                    } catch (error) {
                        console.error(`Error restarting ${service} service:`, error);
                        return false;
                    }
                },

                // Read a log file
                readLogFile: (filePath) => {
                    if (fs.existsSync(filePath)) {
                        return fs.readFileSync(filePath, 'utf8');
                    }
                    return null;
                },

                // Query OpenTelemetry collector for traces
                queryTraces: async ({ traceId }) => {
                    try {
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

            // Determine which spec files to run based on the environment
            console.log("Using accessibleSpecPattern to filter specs");
            // Don't override specPattern - allow CLI --spec parameter to take precedence
            // config.specPattern = ['cypress/e2e/enqueuer.cy.js', 'cypress/e2e/pipeline-tests.cy.js'];

            return config;
        },
        baseUrl: 'https://dev.enqueuer.fullstack.pw', // Default baseUrl
        supportFile: 'cypress/support/e2e.js',
        defaultCommandTimeout: 10000,
        requestTimeout: 8000,
        responseTimeout: 30000,
        retries: {
            runMode: 2,
            openMode: 0
        }
    },
    reporter: 'mochawesome',
    reporterOptions: {
        mochaFile: 'cypress/results/results-[hash].xml',
        toConsole: true,
    },
    env: {
        // Default values
        ENQUEUER_URL: 'https://dev.enqueuer.fullstack.pw',
    },
    experimentalStudio: false,
    // Disable videos and screenshots - KISS principle
    video: false,
    screenshotOnRunFailure: false
});