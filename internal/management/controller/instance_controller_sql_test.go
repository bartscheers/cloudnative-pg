/*
Copyright © contributors to CloudNativePG, established as
CloudNativePG a Series of LF Projects, LLC.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

SPDX-License-Identifier: Apache-2.0
*/

package controller

import (
	"database/sql"

	"github.com/DATA-DOG/go-sqlmock"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	apiv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	schemeBuilder "github.com/cloudnative-pg/cloudnative-pg/internal/scheme"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/management/postgres"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("reconcileSuperuserPassword", func() {
	const (
		namespace  = "default"
		secretName = "superuser-secret"
	)

	var (
		dbMock sqlmock.Sqlmock
		db     *sql.DB
		err    error

		reconciler *InstanceReconciler
		cluster    *apiv1.Cluster
		secretRV   string
	)

	BeforeEach(func(ctx SpecContext) {
		db, dbMock, err = sqlmock.New()
		Expect(err).ToNot(HaveOccurred())

		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: namespace},
			Data: map[string][]byte{
				corev1.BasicAuthUsernameKey: []byte("postgres"),
				corev1.BasicAuthPasswordKey: []byte("supersecret"),
			},
		}
		fakeClient := fake.NewClientBuilder().
			WithScheme(schemeBuilder.BuildWithAllKnownScheme()).
			WithObjects(secret).
			Build()

		// Read back the resource version assigned by the fake client so we can
		// pre-seed the in-memory cache as if the password had already been applied.
		applied := &corev1.Secret{}
		Expect(fakeClient.Get(ctx, client.ObjectKeyFromObject(secret), applied)).To(Succeed())
		secretRV = applied.ResourceVersion
		Expect(secretRV).NotTo(BeEmpty())

		reconciler = &InstanceReconciler{
			client: fakeClient,
			instance: postgres.NewInstance().
				WithNamespace(namespace).
				WithPodName("cluster-example-1").
				WithClusterName("cluster-example"),
			secretVersions: map[string]string{
				// the superuser password was already applied during a previous reconcile
				secretName: secretRV,
			},
		}

		cluster = &apiv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-example", Namespace: namespace},
			Spec: apiv1.ClusterSpec{
				SuperuserSecret: &apiv1.LocalObjectReference{Name: secretName},
			},
		}
	})

	AfterEach(func() {
		Expect(dbMock.ExpectationsWereMet()).To(Succeed())
	})

	It("re-applies the superuser password when access is re-enabled after being disabled", func(ctx SpecContext) {
		// 1) Disable superuser access: the password is dropped in PostgreSQL.
		disabled := false
		cluster.Spec.EnableSuperuserAccess = &disabled

		dbMock.ExpectQuery("SELECT rolpassword IS NOT NULL").
			WillReturnRows(sqlmock.NewRows([]string{"has_password"}).AddRow(true))
		dbMock.ExpectBegin()
		dbMock.ExpectExec("ALTER ROLE postgres WITH PASSWORD NULL").
			WillReturnResult(sqlmock.NewResult(0, 1))
		dbMock.ExpectCommit()

		Expect(reconciler.reconcileSuperuserPassword(ctx, cluster, db)).To(Succeed())

		// 2) Re-enable superuser access. The secret is unchanged (same resource
		//    version), so without invalidating the cache the password would never
		//    be re-applied (this is the bug from #9721). We therefore expect the
		//    ALTER ROLE ... WITH PASSWORD statement to run again.
		enabled := true
		cluster.Spec.EnableSuperuserAccess = &enabled

		dbMock.ExpectBegin()
		dbMock.ExpectExec("SET LOCAL log_statement").WillReturnResult(sqlmock.NewResult(0, 0))
		dbMock.ExpectExec("SET LOCAL log_min_error_statement").WillReturnResult(sqlmock.NewResult(0, 0))
		dbMock.ExpectExec("ALTER ROLE").WillReturnResult(sqlmock.NewResult(0, 1))
		dbMock.ExpectCommit()

		Expect(reconciler.reconcileSuperuserPassword(ctx, cluster, db)).To(Succeed())
	})
})
