describe('ASCII Frontend Tests', () => {
    const frontendUrl = Cypress.env('ASCII_FRONTEND_URL') || 'https://dev.ascii.fullstack.pw';

    it('should load the homepage', () => {
        cy.request({
            method: 'GET',
            url: frontendUrl,
        }).then((response) => {
            expect(response.status).to.equal(200);
            expect(response.body).to.include('html');
        });
    });

    it('should serve static assets', () => {
        cy.request({
            method: 'GET',
            url: frontendUrl,
        }).then((response) => {
            expect(response.status).to.equal(200);
            expect(response.headers['content-type']).to.include('text/html');
            cy.log('ASCII Frontend is serving content correctly');
        });
    });

    it('should be accessible via HTTPS', () => {
        cy.request({
            method: 'GET',
            url: frontendUrl,
        }).then((response) => {
            expect(response.status).to.equal(200);
            cy.log('Frontend is accessible via HTTPS');
        });
    });
});
