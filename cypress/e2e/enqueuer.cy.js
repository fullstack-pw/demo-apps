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
            content: `Linux Tux`,
            id: `valid-${Cypress._.random(0, 1000000)}`
        };

        cy.request({
            method: 'POST',
            url: `${Cypress.env('ENQUEUER_URL')}/add?queue=queue-dev`,
            body: validMessage,
            headers: {
                'Content-Type': 'application/json'
            }
        }).then((response) => {
            expect(response.status).to.equal(201);
            expect(response.body).to.have.property('status');
            expect(response.body).to.have.property('queue', 'queue-dev');
            expect(response.body).to.have.property('image_url').and.not.be.empty;
            expect(response.body).to.have.property('image_ascii_text').and.not.be.empty;
            expect(response.body).to.have.property('image_ascii_html').and.not.be.empty;

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

    it('should check NATS connection status', () => {
        cy.request({
            method: 'GET',
            url: `${Cypress.env('ENQUEUER_URL')}/natscheck`,
        }).then((response) => {
            expect(response.status).to.equal(200);
            expect(response.body).to.include('NATS connection OK');
        });
    });

});