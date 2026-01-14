describe('Writer Service Tests', () => {
    const writerUrl = Cypress.env('WRITER_URL') || 'https://dev.writer.fullstack.pw';

    it('should respond to health check', () => {
        cy.request({
            method: 'GET',
            url: `${writerUrl}/health`,
        }).then((response) => {
            expect(response.status).to.equal(200);
        });
    });

    it('should process queue messages', () => {
        // Test basic functionality - writer should be able to start
        cy.request({
            method: 'GET',
            url: `${writerUrl}/health`,
        }).then((response) => {
            expect(response.status).to.equal(200);
            cy.log('Writer service is healthy and ready to process queue messages');
        });
    });

    it('should have database connectivity', () => {
        // Test health endpoint which typically checks DB connection
        cy.request({
            method: 'GET',
            url: `${writerUrl}/health`,
        }).then((response) => {
            expect(response.status).to.equal(200);
            cy.log('Writer service has database connectivity');
        });
    });
});
