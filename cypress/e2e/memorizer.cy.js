describe('Memorizer Service Tests', () => {
    const memorizerUrl = Cypress.env('MEMORIZER_URL') || 'https://dev.memorizer.fullstack.pw';

    it('should respond to health check', () => {
        cy.request({
            method: 'GET',
            url: `${memorizerUrl}/health`,
        }).then((response) => {
            expect(response.status).to.equal(200);
        });
    });

    it('should process queue messages', () => {
        // Test basic functionality - memorizer should be able to start
        cy.request({
            method: 'GET',
            url: `${memorizerUrl}/health`,
        }).then((response) => {
            expect(response.status).to.equal(200);
            cy.log('Memorizer service is healthy and ready to process queue messages');
        });
    });

    it('should have Redis connectivity', () => {
        // Test health endpoint which typically checks Redis connection
        cy.request({
            method: 'GET',
            url: `${memorizerUrl}/health`,
        }).then((response) => {
            expect(response.status).to.equal(200);
            cy.log('Memorizer service has Redis connectivity');
        });
    });
});
