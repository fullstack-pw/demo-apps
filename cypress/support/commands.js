// This file allows to create custom Cypress commands and overwrite existing ones

// -- This is a parent command --
Cypress.Commands.add('sendMessage', (message, queueName = null) => {
    // Use environment-specific queue name if not explicitly provided
    const defaultQueue = `queue-${Cypress.env('ENVIRONMENT')}`;
    const queue = queueName || defaultQueue;

    return cy.request({
        method: 'POST',
        url: `${Cypress.env('ENQUEUER_URL')}/add?queue=${queue}`,
        body: message,
        headers: {
            'Content-Type': 'application/json'
        }
    });
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

// Command to verify a message was processed by memorizer via Enqueuer
Cypress.Commands.add('verifyMemorizerProcessed', (messageId, timeout = 10000) => {
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

// Command to verify a message was stored by writer via Enqueuer
Cypress.Commands.add('verifyWriterStored', (messageId, timeout = 15000) => {
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

// Command to test the full message pipeline flow
Cypress.Commands.add('testFullPipeline', (messageContent, options = {}) => {
    const testId = `pipeline-${Date.now()}-${Cypress._.random(0, 10000)}`;
    const messageData = {
        id: testId,
        content: messageContent,
        headers: options.headers || {}
    };

    return cy.sendMessage(messageData, options.queue || 'cypress-test')
        .then(sendResponse => {
            expect(sendResponse.status).to.equal(201);

            // Verify memorizer processed the message
            return cy.verifyMemorizerProcessed(testId, options.memorizerTimeout || 10000)
                .then(() => {
                    // Verify writer stored the message
                    return cy.verifyWriterStored(testId, options.writerTimeout || 15000)
                        .then(storedMessage => {
                            // Verify trace context if needed
                            if (options.verifyTrace) {
                                return cy.verifyTraceContext(testId, options.traceTimeout || 5000)
                                    .then(traceId => {
                                        return {
                                            messageId: testId,
                                            initialResponse: sendResponse.body,
                                            storedMessage: storedMessage,
                                            traceId: traceId
                                        };
                                    });
                            }

                            return {
                                messageId: testId,
                                initialResponse: sendResponse.body,
                                storedMessage: storedMessage
                            };
                        });
                });
        });
});