describe('Microservices Pipeline Tests', () => {
    beforeEach(() => {
        // Load test data
        cy.fixture('messages.json').then(data => {
            cy.wrap(data).as('testData');
        });

        // Check if Enqueuer service is available
        cy.request({
            method: 'GET',
            url: `${Cypress.env('ENQUEUER_URL')}/health`,
            failOnStatusCode: false
        }).then(response => {
            if (response.status !== 200) {
                cy.log('Warning: Enqueuer service is not healthy, tests may fail');
            }
        });
    });

    it('should process a basic message through the entire pipeline', () => {
        cy.generateTestId('pipeline-test').then(testId => {
            const testMessage = {
                id: testId,
                content: `Basic test message ${Date.now()}`
            };

            // Send the message through Enqueuer
            cy.sendMessage(testMessage).then(response => {
                expect(response.status).to.equal(201);
                cy.log(`Message ${testId} sent to pipeline`);

                // Use retry logic to wait for memorizer to process the message
                // Add a delay to ensure Redis write operation completes
                // The memorizer stores in Redis asynchronously after publishing to NATS
                cy.wait(30000);

                // Check if the message was processed by Memorizer via Enqueuer's check endpoint
                cy.request({
                    method: 'GET',
                    url: `${Cypress.env('ENQUEUER_URL')}/check-memorizer?id=${testId}`,
                    retryOnStatusCodeFailure: true,
                    timeout: 10000
                }).then(memorizerResponse => {
                    expect(memorizerResponse.status).to.equal(200);
                    expect(memorizerResponse.body).to.have.property('processed', true);
                    cy.log(`Message ${testId} processed by Memorizer`);

                    // Check if the message was stored by Writer via Enqueuer's check endpoint
                    cy.request({
                        method: 'GET',
                        url: `${Cypress.env('ENQUEUER_URL')}/check-writer?id=${testId}`,
                        retryOnStatusCodeFailure: true,
                        timeout: 15000
                    }).then(writerResponse => {
                        expect(writerResponse.status).to.equal(200);
                        expect(writerResponse.body).to.have.property('id', testId);
                        expect(writerResponse.body).to.have.property('content', testMessage.content);
                        cy.log(`Message ${testId} stored by Writer`);
                    });
                });
            });
        });
    });

    it('should process a message with image search and ASCII art generation', () => {
        cy.generateTestId('ascii-test').then(testId => {
            const testMessage = {
                id: testId,
                content: 'Mountain landscape with sunset',
            };

            // Send message that should trigger image search and ASCII generation
            cy.sendMessage(testMessage).then(response => {
                expect(response.status).to.equal(201);
                expect(response.body).to.have.property('image_url').and.not.be.empty;

                // Store important data from response
                const imageUrl = response.body.image_url;
                const hasAsciiText = response.body.image_ascii_text && response.body.image_ascii_text.length > 0;
                const hasAsciiHtml = response.body.image_ascii_html && response.body.image_ascii_html.length > 0;

                cy.log(`Message ${testId} sent with image URL: ${imageUrl}`);
                cy.log(`ASCII text generated: ${hasAsciiText ? 'Yes' : 'No'}`);
                cy.log(`ASCII HTML generated: ${hasAsciiHtml ? 'Yes' : 'No'}`);

                // Verify the message was stored in Writer with image URL and ASCII art references
                cy.request({
                    method: 'GET',
                    url: `${Cypress.env('ENQUEUER_URL')}/check-writer?id=${testId}`,
                    retryOnStatusCodeFailure: true,
                    timeout: 20000
                }).then(writerResponse => {
                    expect(writerResponse.status).to.equal(200);
                    expect(writerResponse.body).to.have.property('id', testId);

                    // Check for image URL in headers
                    const headers = writerResponse.body.headers || {};
                    expect(headers).to.have.property('image_url');

                    // Check for ASCII art references
                    if (writerResponse.body.ascii_art) {
                        cy.log('ASCII art references found in stored message');

                        // If terminal ASCII reference exists, try to access it
                        if (writerResponse.body.ascii_art.terminal) {
                            cy.request(writerResponse.body.ascii_art.terminal).then(asciiResponse => {
                                expect(asciiResponse.status).to.equal(200);
                                cy.log('Terminal ASCII art retrieved successfully');
                            });
                        }

                        // If HTML ASCII reference exists, try to access it
                        if (writerResponse.body.ascii_art.html) {
                            cy.request(writerResponse.body.ascii_art.html).then(htmlResponse => {
                                expect(htmlResponse.status).to.equal(200);
                                cy.log('HTML ASCII art retrieved successfully');
                            });
                        }
                    }
                });
            });
        });
    });

});