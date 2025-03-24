// This file allows you to create custom Cypress commands and overwrite existing ones

// -- This is a parent command --
Cypress.Commands.add('sendMessage', (message, queueName = 'cypress-test') => {
    return cy.request({
        method: 'POST',
        url: `${Cypress.env('ENQUEUER_URL')}/add?queue=${queueName}`,
        body: message,
        headers: {
            'Content-Type': 'application/json'
        }
    });
});

// Command to verify a message was processed by memorizer
Cypress.Commands.add('verifyMemorizerProcessed', (messageId, timeout = 5000) => {
    // Use polling to check if the message has been processed
    const checkInterval = 500; // ms
    const maxAttempts = timeout / checkInterval;
    let attempts = 0;

    const checkStatus = () => {
        return cy.request({
            method: 'GET',
            url: `${Cypress.env('ENQUEUER_URL')}/check-memorizer?id=${messageId}`,
            failOnStatusCode: false
        }).then(response => {
            if (response.status === 200 && response.body.processed) {
                return true;
            } else if (attempts < maxAttempts) {
                attempts++;
                // Wait and try again
                cy.wait(checkInterval);
                return checkStatus();
            } else {
                // Timeout reached
                throw new Error(`Message ${messageId} not processed after ${timeout}ms`);
            }
        });
    };

    return checkStatus();
});

Cypress.Commands.add('verifyWriterStored', (messageId, timeout = 8000) => {
    // Use polling to check if the message has been written
    const checkInterval = 500; // ms
    const maxAttempts = timeout / checkInterval;
    let attempts = 0;

    const checkDatabase = () => {
        return cy.request({
            method: 'GET',
            url: `${Cypress.env('ENQUEUER_URL')}/check-writer?id=${messageId}`,
            failOnStatusCode: false
        }).then(response => {
            if (response.status === 200 && response.body.id === messageId) {
                return response.body;
            } else if (attempts < maxAttempts) {
                attempts++;
                // Wait and try again
                cy.wait(checkInterval);
                return checkDatabase();
            } else {
                // Timeout reached
                throw new Error(`Message ${messageId} not found in database after ${timeout}ms`);
            }
        });
    };

    return checkDatabase();
});


// Command to check service health
Cypress.Commands.add('checkServiceHealth', (service) => {
    return cy.request({
        method: 'GET',
        url: `${Cypress.env(service.toUpperCase() + '_URL')}/health`,
    }).then(response => {
        expect(response.status).to.equal(200);
        return response;
    });
});

// Command to generate unique test IDs
Cypress.Commands.add('generateTestId', (prefix = 'test') => {
    const timestamp = Date.now();
    const random = Math.floor(Math.random() * 10000);
    return `${prefix}-${timestamp}-${random}`;
});

// Command to verify trace context propagation
Cypress.Commands.add('verifyTraceContext', (messageId, timeout = 10000) => {
    const checkInterval = 1000; // ms
    const maxAttempts = timeout / checkInterval;
    let attempts = 0;

    const checkTrace = () => {
        return cy.request({
            method: 'GET',
            url: `${Cypress.env('ENQUEUER_URL')}/check-trace?id=${messageId}`,
            failOnStatusCode: false
        }).then(response => {
            if (response.status === 200 && response.body.trace_id) {
                return response.body.trace_id;
            } else if (attempts < maxAttempts) {
                attempts++;
                // Wait and try again
                cy.wait(checkInterval);
                return checkTrace();
            } else {
                // Timeout reached
                throw new Error(`Trace context for message ${messageId} not found after ${timeout}ms`);
            }
        });
    };

    return checkTrace();
});