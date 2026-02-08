describe('CKS Terminal Management Tests', () => {
    const apiUrl = Cypress.env('CKS_TERMINAL_MGMT_URL') || 'https://dev.terminal.cks.fullstack.pw';

    it('should respond to health check', () => {
        cy.request({
            method: 'GET',
            url: `${apiUrl}/health`,
            failOnStatusCode: false,
        }).then((response) => {
            expect(response.status).to.equal(200);
            expect(response.body).to.have.property('status', 'ok');
            expect(response.body).to.have.property('active_sessions');
            expect(response.body.active_sessions).to.be.a('number');
        });
    });

    it('should expose Prometheus metrics', () => {
        cy.request({
            method: 'GET',
            url: `${apiUrl}/metrics`,
            failOnStatusCode: false,
        }).then((response) => {
            expect(response.status).to.equal(200);
            expect(response.body).to.include('go_');
        });
    });

    it('should return 400 when vmIP parameter is missing', () => {
        cy.request({
            method: 'GET',
            url: `${apiUrl}/terminal`,
            failOnStatusCode: false,
        }).then((response) => {
            expect(response.status).to.equal(400);
            expect(response.body).to.include('vmIP query parameter required');
        });
    });

    it('should return 400 when vmIP is invalid', () => {
        cy.request({
            method: 'GET',
            url: `${apiUrl}/terminal?vmIP=notanip`,
            failOnStatusCode: false,
        }).then((response) => {
            expect(response.status).to.equal(400);
            expect(response.body).to.include('invalid vmIP');
        });
    });
});
