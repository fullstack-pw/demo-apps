// Tests specific to the Writer service
describe('Writer Service Tests', () => {
    beforeEach(() => {
        // Set up test data or prerequisites if needed
    });

    it('should have a healthy database connection', () => {
        cy.request({
            method: 'GET',
            url: `${Cypress.env('WRITER_URL')}/dbcheck`,
        }).then((response) => {
            expect(response.status).to.equal(200);
            expect(response.body).to.include('PostgreSQL connection OK');
        });
    });

    it('should have a healthy Redis connection', () => {
        cy.request({
            method: 'GET',
            url: `${Cypress.env('WRITER_URL')}/redischeck`,
        }).then((response) => {
            expect(response.status).to.equal(200);
            expect(response.body).to.include('Redis connection OK');
        });
    });

    it('should retrieve previously written messages', () => {
        // First, ensure a message exists in the system by sending it through the pipeline
        const testMessage = {
            content: `Writer retrieval test ${Date.now()}`,
            id: `writer-${Cypress._.random(0, 1000000)}`
        };

        // Send message through enqueuer
        cy.request({
            method: 'POST',
            url: `${Cypress.env('ENQUEUER_URL')}/add?queue=writer-test`,
            body: testMessage,
            headers: {
                'Content-Type': 'application/json'
            }
        }).then((response) => {
            expect(response.status).to.equal(201);
        });

        // Wait for message to be processed through the pipeline
        cy.wait(5000);

        // Now query the writer service to verify it was written to the database
        cy.request({
            method: 'GET',
            url: `${Cypress.env('WRITER_URL')}/query?id=${testMessage.id}`,
            failOnStatusCode: false
        }).then((response) => {
            expect(response.status).to.equal(200);
            expect(response.body).to.have.property('id', testMessage.id);
            expect(response.body).to.have.property('content', testMessage.content);
            expect(response.body).to.have.property('source', 'redis');
        });
    });

    it('should handle database query errors gracefully', () => {
        // Test with an invalid query parameter
        cy.request({
            method: 'GET',
            url: `${Cypress.env('WRITER_URL')}/query?invalid=parameter`,
            failOnStatusCode: false
        }).then((response) => {
            // Should return a proper error response, not crash
            expect(response.status).to.be.within(400, 500);
        });
    });

    it('should persist data across service restarts', () => {
        // This test may require additional endpoints or tasks to simulate service restart
        // In a real environment, you might need to use task plugins to restart services

        const testMessage = {
            content: `Persistence test ${Date.now()}`,
            id: `persist-${Cypress._.random(0, 1000000)}`
        };

        // Send message through enqueuer
        cy.request({
            method: 'POST',
            url: `${Cypress.env('ENQUEUER_URL')}/add?queue=persist-test`,
            body: testMessage,
            headers: {
                'Content-Type': 'application/json'
            }
        }).then((response) => {
            expect(response.status).to.equal(201);
        });

        // Wait for processing
        cy.wait(5000);

        // Verify data is available before simulated restart
        cy.request({
            method: 'GET',
            url: `${Cypress.env('WRITER_URL')}/query?id=${testMessage.id}`,
            failOnStatusCode: false
        }).then((response) => {
            expect(response.status).to.equal(200);
            expect(response.body).to.have.property('id', testMessage.id);
        });

        // Simulate service restart (this would need a custom task)
        cy.task('simulateServiceRestart', { service: 'writer' }, { log: false });

        // Verify data is still available after restart
        cy.wait(3000); // Wait for service to come back online
        cy.request({
            method: 'GET',
            url: `${Cypress.env('WRITER_URL')}/query?id=${testMessage.id}`,
            failOnStatusCode: false
        }).then((response) => {
            expect(response.status).to.equal(200);
            expect(response.body).to.have.property('id', testMessage.id);
        });
    });
});