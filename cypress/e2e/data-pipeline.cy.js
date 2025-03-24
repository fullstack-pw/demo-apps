// E2E test for the entire data pipeline
describe('Data Pipeline End-to-End Tests', () => {
    beforeEach(() => {
        // Reset any test state if needed
        cy.task('resetTestState', {}, { log: false });
    });

    it('should process a message through the entire pipeline', () => {
        const testMessage = {
            content: `Test message ${Date.now()}`,
            id: `test-${Cypress._.random(0, 1000000)}`
        };

        // Step 1: Send message to enqueuer
        cy.request({
            method: 'POST',
            url: `${Cypress.env('ENQUEUER_URL')}/add?queue=cypress-test`,
            body: testMessage,
            headers: {
                'Content-Type': 'application/json'
            }
        }).then((response) => {
            // Verify enqueuer response
            expect(response.status).to.equal(201);
            expect(response.body).to.have.property('status', 'queued');
            expect(response.body).to.have.property('queue', 'cypress-test');

            // Store message ID for later verification
            cy.wrap(testMessage.id).as('messageId');
        });

        // Step 2: Verify message was processed by memorizer
        // This may require a polling mechanism as processing is asynchronous
        cy.wait(2000); // Wait for message processing
        cy.request({
            method: 'GET',
            url: `${Cypress.env('MEMORIZER_URL')}/status?id=${testMessage.id}`,
            failOnStatusCode: false
        }).then((response) => {
            expect(response.status).to.equal(200);
            expect(response.body).to.have.property('processed', true);
        });

        // Step 3: Verify message was written to PostgreSQL by writer
        cy.wait(3000); // Wait for write operations
        cy.request({
            method: 'GET',
            url: `${Cypress.env('WRITER_URL')}/query?id=${testMessage.id}`,
            failOnStatusCode: false
        }).then((response) => {
            expect(response.status).to.equal(200);
            expect(response.body).to.have.property('id', testMessage.id);
            expect(response.body).to.have.property('content', testMessage.content);
        });
    });

    it('should handle errors and retries appropriately', () => {
        // Test with a message that will trigger error handling
        const errorMessage = {
            content: 'Error test message',
            id: `error-${Cypress._.random(0, 1000000)}`,
            triggerError: true
        };

        // Send message that will cause an error
        cy.request({
            method: 'POST',
            url: `${Cypress.env('ENQUEUER_URL')}/add?queue=cypress-test`,
            body: errorMessage,
            headers: {
                'Content-Type': 'application/json'
            }
        }).then((response) => {
            expect(response.status).to.equal(201);
        });

        // Check error logs or monitoring endpoint
        cy.wait(2000);
        cy.request({
            method: 'GET',
            url: `${Cypress.env('ENQUEUER_URL')}/health`,
        }).then((response) => {
            expect(response.status).to.equal(200);
        });

        // Verify the error was handled and system is still operational
        cy.request({
            method: 'GET',
            url: `${Cypress.env('WRITER_URL')}/health`,
        }).then((response) => {
            expect(response.status).to.equal(200);
        });
    });

    it('should propagate trace context through the pipeline', () => {
        // This test may require additional endpoints to expose trace information
        const testMessage = {
            content: `Trace test ${Date.now()}`,
            id: `trace-${Cypress._.random(0, 1000000)}`
        };

        // Send message with trace headers
        cy.request({
            method: 'POST',
            url: `${Cypress.env('ENQUEUER_URL')}/add?queue=cypress-test`,
            body: testMessage,
            headers: {
                'Content-Type': 'application/json',
                'X-Test-Trace': 'true'
            }
        }).then((response) => {
            expect(response.status).to.equal(201);
        });

        // Check if trace ID was propagated through the system
        cy.wait(5000); // Wait for processing to complete
        cy.request({
            method: 'GET',
            url: `${Cypress.env('WRITER_URL')}/traces?id=${testMessage.id}`,
            failOnStatusCode: false
        }).then((response) => {
            expect(response.status).to.equal(200);
            expect(response.body).to.have.property('trace_id');
            expect(response.body).to.have.property('propagated', true);
        });
    });
});