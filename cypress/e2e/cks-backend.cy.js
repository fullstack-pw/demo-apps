describe('CKS Backend API Tests', () => {
    const apiUrl = Cypress.env('CKS_BACKEND_URL') || 'https://dev.api.cks.fullstack.pw';

    it('should respond to health check', () => {
        cy.request({
            method: 'GET',
            url: `${apiUrl}/health`,
            failOnStatusCode: false,
        }).then((response) => {
            expect(response.status).to.equal(200);
            expect(response.body).to.have.property('status', 'healthy');
        });
    });

    it('should list available scenarios', () => {
        cy.request({
            method: 'GET',
            url: `${apiUrl}/api/v1/scenarios`,
            failOnStatusCode: false,
        }).then((response) => {
            expect(response.status).to.equal(200);
            expect(response.body).to.be.an('array');
        });
    });

    it('should create a new session', () => {
        cy.request({
            method: 'POST',
            url: `${apiUrl}/api/v1/sessions`,
            body: {
                scenarioId: 'test-scenario'
            },
            failOnStatusCode: false,
        }).then((response) => {
            expect(response.status).to.be.oneOf([200, 201]);
            expect(response.body).to.have.property('id');
        });
    });

    it('should get admin cluster pool status', () => {
        cy.request({
            method: 'GET',
            url: `${apiUrl}/api/v1/admin/cluster-pool/status`,
            failOnStatusCode: false,
        }).then((response) => {
            expect(response.status).to.equal(200);
            expect(response.body).to.have.property('available');
            expect(response.body).to.have.property('inUse');
            expect(response.body).to.have.property('total');
        });
    });

    it('should list tasks for a scenario', () => {
        cy.request({
            method: 'GET',
            url: `${apiUrl}/api/v1/scenarios/test-scenario/tasks`,
            failOnStatusCode: false,
        }).then((response) => {
            expect(response.status).to.be.oneOf([200, 404]);
            if (response.status === 200) {
                expect(response.body).to.be.an('array');
            }
        });
    });
});
