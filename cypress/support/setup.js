// Test setup and teardown utilities

// Load environment configuration
const loadEnvironmentConfig = (environment) => {
    return cy.fixture('environments.json').then(environments => {
        const config = environments[environment] || environments.dev;

        // Set environment URLs as Cypress environment variables
        Cypress.env('ENQUEUER_URL', config.enqueuer);
        Cypress.env('MEMORIZER_URL', config.memorizer);
        Cypress.env('WRITER_URL', config.writer);

        return config;
    });
};

// Clean up test data
const cleanupTestData = (messageIds) => {
    if (!messageIds || messageIds.length === 0) {
        return;
    }

    // Delete messages from database
    messageIds.forEach(id => {
        cy.request({
            method: 'DELETE',
            url: `${Cypress.env('WRITER_URL')}/cleanup?id=${id}`,
            failOnStatusCode: false
        });
    });
};

// Reset services to clean state
const resetServices = () => {
    // This function would trigger any cleanup endpoints in your services
    // For example, clearing test queues or resetting connections
    return Promise.all([
        cy.request({
            method: 'POST',
            url: `${Cypress.env('ENQUEUER_URL')}/reset-test-state`,
            failOnStatusCode: false
        }),
        cy.request({
            method: 'POST',
            url: `${Cypress.env('MEMORIZER_URL')}/reset-test-state`,
            failOnStatusCode: false
        }),
        cy.request({
            method: 'POST',
            url: `${Cypress.env('WRITER_URL')}/reset-test-state`,
            failOnStatusCode: false
        })
    ]);
};

// Wait for all services to be healthy
const waitForServicesHealth = (timeout = 30000) => {
    const services = ['enqueuer', 'memorizer', 'writer'];
    const checkInterval = 1000; // ms
    let attempts = 0;
    const maxAttempts = timeout / checkInterval;

    const checkHealth = () => {
        return Promise.all(
            services.map(service => {
                return cy.request({
                    method: 'GET',
                    url: `${Cypress.env(service.toUpperCase() + '_URL')}/health`,
                    failOnStatusCode: false
                }).then(response => ({
                    service,
                    healthy: response.status === 200
                }));
            })
        ).then(results => {
            const allHealthy = results.every(r => r.healthy);
            if (allHealthy) {
                return true;
            } else if (attempts < maxAttempts) {
                attempts++;
                cy.wait(checkInterval);
                return checkHealth();
            } else {
                const unhealthy = results.filter(r => !r.healthy).map(r => r.service);
                throw new Error(`Services not healthy after ${timeout}ms: ${unhealthy.join(', ')}`);
            }
        });
    };

    return checkHealth();
};

// Export utilities
export {
    loadEnvironmentConfig,
    cleanupTestData,
    resetServices,
    waitForServicesHealth
};