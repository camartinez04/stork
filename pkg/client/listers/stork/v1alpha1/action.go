/*
Copyright 2018 Openstorage.org

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Code generated by lister-gen. DO NOT EDIT.

package v1alpha1

import (
	v1alpha1 "github.com/libopenstorage/stork/pkg/apis/stork/v1alpha1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
)

// ActionLister helps list Actions.
// All objects returned here must be treated as read-only.
type ActionLister interface {
	// List lists all Actions in the indexer.
	// Objects returned here must be treated as read-only.
	List(selector labels.Selector) (ret []*v1alpha1.Action, err error)
	// Actions returns an object that can list and get Actions.
	Actions(namespace string) ActionNamespaceLister
	ActionListerExpansion
}

// actionLister implements the ActionLister interface.
type actionLister struct {
	indexer cache.Indexer
}

// NewActionLister returns a new ActionLister.
func NewActionLister(indexer cache.Indexer) ActionLister {
	return &actionLister{indexer: indexer}
}

// List lists all Actions in the indexer.
func (s *actionLister) List(selector labels.Selector) (ret []*v1alpha1.Action, err error) {
	err = cache.ListAll(s.indexer, selector, func(m interface{}) {
		ret = append(ret, m.(*v1alpha1.Action))
	})
	return ret, err
}

// Actions returns an object that can list and get Actions.
func (s *actionLister) Actions(namespace string) ActionNamespaceLister {
	return actionNamespaceLister{indexer: s.indexer, namespace: namespace}
}

// ActionNamespaceLister helps list and get Actions.
// All objects returned here must be treated as read-only.
type ActionNamespaceLister interface {
	// List lists all Actions in the indexer for a given namespace.
	// Objects returned here must be treated as read-only.
	List(selector labels.Selector) (ret []*v1alpha1.Action, err error)
	// Get retrieves the Action from the indexer for a given namespace and name.
	// Objects returned here must be treated as read-only.
	Get(name string) (*v1alpha1.Action, error)
	ActionNamespaceListerExpansion
}

// actionNamespaceLister implements the ActionNamespaceLister
// interface.
type actionNamespaceLister struct {
	indexer   cache.Indexer
	namespace string
}

// List lists all Actions in the indexer for a given namespace.
func (s actionNamespaceLister) List(selector labels.Selector) (ret []*v1alpha1.Action, err error) {
	err = cache.ListAllByNamespace(s.indexer, s.namespace, selector, func(m interface{}) {
		ret = append(ret, m.(*v1alpha1.Action))
	})
	return ret, err
}

// Get retrieves the Action from the indexer for a given namespace and name.
func (s actionNamespaceLister) Get(name string) (*v1alpha1.Action, error) {
	obj, exists, err := s.indexer.GetByKey(s.namespace + "/" + name)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, errors.NewNotFound(v1alpha1.Resource("action"), name)
	}
	return obj.(*v1alpha1.Action), nil
}
