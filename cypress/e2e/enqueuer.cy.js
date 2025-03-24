// Tests specific to the Enqueuer service
describe('Enqueuer Service Tests', () => {
    it('should accept and queue valid messages', () => {
        // Test with valid message
        const validMessage = {
            content: `Valid test message ${Date.now()}`,
            id: `valid-${Cypress._.random(0, 1000000)}`
        };

        cy.request({
            method: 'POST',
            url: `${Cypress.env('ENQUEUER_URL')}/add?queue=test-queue`,
            body: validMessage,
            headers: {
                'Content-Type': 'application/json'
            }
        }).then((response) => {
            expect(response.status).to.equal(201);
            expect(response.body).to.have.property('status', 'queued');
            expect(response.body).to.have.property('queue', 'test-queue');
        });
    });

    it('should reject invalid message formats', () => {
        // Test with invalid message
        const invalidMessage = "This is not JSON";

        cy.request({
            method: 'POST',
            url: `${Cypress.env('ENQUEUER_URL')}/add`,
            body: invalidMessage,
            headers: {
                'Content-Type': 'text/plain'
            },
            failOnStatusCode: false
        }).then((response) => {
            expect(response.status).to.equal(400);
        });
    });

    it('should handle high volume of messages', () => {
        // Send multiple messages in quick succession
        const messageCount = 10;
        const promises = [];

        for (let i = 0; i < messageCount; i++) {
            const message = {
                content: `Bulk test message ${i}`,
                id: `bulk-${i}-${Cypress._.random(0, 1000000)}`
            };

            promises.push(
                cy.request({
                    method: 'POST',
                    url: `${Cypress.env('ENQUEUER_URL')}/add?queue=bulk-test`,
                    body: message,
                    headers: {
                        'Content-Type': 'application/json'
                    }
                }).then((response) => {
                    expect(response.status).to.equal(201);
                })
            );
        }

        // Wait for all requests to complete
        cy.wrap(promises).then(() => {
            // Check service health after bulk operation
            cy.request({
                method: 'GET',
                url: `${Cypress.env('ENQUEUER_URL')}/health`,
            }).then((response) => {
                expect(response.status).to.equal(200);
            });
        });
    });

    it('should check NATS connection status', () => {
        cy.request({
            method: 'GET',
            url: `${Cypress.env('ENQUEUER_URL')}/natscheck`,
        }).then((response) => {
            expect(response.status).to.equal(200);
            expect(response.body).to.include('NATS connection OK');
        });
    });

    it('should include proper headers in response', () => {
        const message = {
            content: 'Header test message',
            id: `header-${Cypress._.random(0, 1000000)}`
        };

        cy.request({
            method: 'POST',
            url: `${Cypress.env('ENQUEUER_URL')}/add`,
            body: message,
            headers: {
                'Content-Type': 'application/json'
            }
        }).then((response) => {
            expect(response.status).to.equal(201);
            expect(response.headers).to.have.property('content-type').that.includes('application/json');
        });
    });
});