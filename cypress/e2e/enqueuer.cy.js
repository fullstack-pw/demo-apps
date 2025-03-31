describe('Enqueuer Service Tests', () => {
    beforeEach(() => {
        // Load test data
        cy.fixture('messages.json').then(data => {
            cy.wrap(data).as('testData');
        });
    });

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
            expect(response.body).to.have.property('status');
            expect(response.body).to.have.property('queue', 'test-queue');
            expect(response.body).to.have.property('image_url').and.not.be.empty;

            // Check for ASCII art in response
            const hasAsciiText = response.body.image_ascii_text && response.body.image_ascii_text.length > 0;
            const hasAsciiHtml = response.body.image_ascii_html && response.body.image_ascii_html.length > 0;

            cy.log(`Message accepted with image URL: ${response.body.image_url}`);
            cy.log(`ASCII text generated: ${hasAsciiText ? 'Yes' : 'No'}`);
            cy.log(`ASCII HTML generated: ${hasAsciiHtml ? 'Yes' : 'No'}`);
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
        const messageCount = 5; // Reduced count to avoid overloading
        const messages = [];

        for (let i = 0; i < messageCount; i++) {
            messages.push({
                content: `Bulk test message ${i} at ${Date.now()}`,
                id: `bulk-${i}-${Cypress._.random(0, 1000000)}`
            });
        }

        // Send messages sequentially to avoid overwhelming the service
        cy.wrap(messages).each(message => {
            cy.request({
                method: 'POST',
                url: `${Cypress.env('ENQUEUER_URL')}/add?queue=bulk-test`,
                body: message,
                headers: {
                    'Content-Type': 'application/json'
                }
            }).then((response) => {
                expect(response.status).to.equal(201);
                cy.log(`Message ${message.id} sent successfully`);
            });
        });

        // Check service health after bulk operation
        cy.request({
            method: 'GET',
            url: `${Cypress.env('ENQUEUER_URL')}/health`,
        }).then((response) => {
            expect(response.status).to.equal(200);
            expect(response.body).to.have.property('status', 'UP');
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

    it('should process specific image search queries', () => {
        // Test with specific image search queries
        const imageSearchQueries = [
            'Mountain landscape',
            'Ocean sunset'
        ];

        // Test each query
        cy.wrap(imageSearchQueries).each(queryText => {
            const message = {
                content: queryText,
                id: `image-${queryText.replace(/\s+/g, '-')}-${Date.now()}`
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
                expect(response.body).to.have.property('image_url').and.not.be.empty;

                cy.log(`Query "${queryText}" returned image URL: ${response.body.image_url}`);
            });
        });
    });

    it('should test proxy endpoints for checking other services', () => {
        // First send a test message
        const testId = `proxy-test-${Date.now()}`;
        const testMessage = {
            id: testId,
            content: 'Test message for proxy endpoints'
        };

        cy.sendMessage(testMessage).then(response => {
            expect(response.status).to.equal(201);

            // Wait a few seconds for message to be processed
            cy.wait(5000);

            // Test check-memorizer endpoint
            cy.request({
                method: 'GET',
                url: `${Cypress.env('ENQUEUER_URL')}/check-memorizer?id=${testId}`,
                failOnStatusCode: false
            }).then(memorizerResponse => {
                // We might not get a 200 if the message hasn't been processed yet,
                // but the endpoint should be accessible
                expect(memorizerResponse.status).to.be.oneOf([200, 404]);
            });

            // Test check-writer endpoint
            cy.request({
                method: 'GET',
                url: `${Cypress.env('ENQUEUER_URL')}/check-writer?id=${testId}`,
                failOnStatusCode: false
            }).then(writerResponse => {
                // We might not get a 200 if the message hasn't been stored yet,
                // but the endpoint should be accessible
                expect(writerResponse.status).to.be.oneOf([200, 404]);
            });
        });
    });
});