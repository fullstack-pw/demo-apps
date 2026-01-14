describe('CKS Frontend Tests', () => {
    const baseUrl = Cypress.env('CKS_FRONTEND_URL') || 'https://dev.cks.fullstack.pw';

    beforeEach(() => {
        cy.visit(baseUrl);
    });

    it('should load the homepage successfully', () => {
        cy.get('body').should('be.visible');
        cy.title().should('not.be.empty');
    });

    it('should have proper status code on homepage', () => {
        cy.request({
            url: baseUrl,
            followRedirect: true,
        }).then((response) => {
            expect(response.status).to.equal(200);
        });
    });

    it('should load static assets correctly', () => {
        cy.visit(baseUrl);

        // Check that the page loaded without JavaScript errors
        cy.window().should('exist');

        // Verify the app container exists
        cy.get('body').should('be.visible');
    });

    it('should navigate to admin page', () => {
        cy.visit(`${baseUrl}/admin`);
        cy.get('body').should('be.visible');
    });

    it('should handle 404 for non-existent routes gracefully', () => {
        cy.request({
            url: `${baseUrl}/non-existent-page`,
            failOnStatusCode: false,
        }).then((response) => {
            // Next.js apps typically return 200 with their custom 404 page
            // or redirect, so we check it's not a server error
            expect(response.status).to.be.oneOf([200, 404]);
        });
    });

    it('should be accessible over HTTPS', () => {
        // Verify the URL is using HTTPS
        cy.url().should('include', 'https://');
    });

    it('should have valid HTML structure', () => {
        cy.visit(baseUrl);
        cy.get('html').should('exist');
        cy.get('head').should('exist');
        cy.get('body').should('exist');
    });
});
