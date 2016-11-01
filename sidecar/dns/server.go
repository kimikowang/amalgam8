// Copyright 2016 IBM Corporation
//
//   Licensed under the Apache License, Version 2.0 (the "License");
//   you may not use this file except in compliance with the License.
//   You may obtain a copy of the License at
//
//       http://www.apache.org/licenses/LICENSE-2.0
//
//   Unless required by applicable law or agreed to in writing, software
//   distributed under the License is distributed on an "AS IS" BASIS,
//   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//   See the License for the specific language governing permissions and
//   limitations under the License.

package dns

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/Sirupsen/logrus"
	"github.com/amalgam8/amalgam8/registry/client"
	"github.com/miekg/dns"
	"math/rand"
)

// Server represent a DNS server. has config field for port,domain,and client discovery, and the DNS server itself
type Server struct {
	config    Config
	dnsServer *dns.Server
}

// Config represents the DNS server configurations.
type Config struct {
	DiscoveryClient client.Discovery
	Port            uint16
	Domain          string
}

// NewServer creates a new instance of a DNS server with the given configurations
func NewServer(config Config) (*Server, error) {
	err := validate(&config)
	if err != nil {
		return nil, err
	}
	s := &Server{
		config: config,
	}

	// Setup DNS muxing
	mux := dns.NewServeMux()
	mux.HandleFunc(config.Domain, s.handleRequest)

	// Setup a DNS server
	s.dnsServer = &dns.Server{
		Addr:    fmt.Sprintf(":%d", config.Port),
		Net:     "udp",
		Handler: mux,
	}

	return s, nil
}

// ListenAndServe starts the DNS server
func (s *Server) ListenAndServe() error {
	logrus.Info("Starting DNS server")
	err := s.dnsServer.ListenAndServe()

	if err != nil {
		logrus.WithError(err).Errorf("Error starting DNS server")
	}

	return nil
}

// Shutdown stops the DNS server
func (s *Server) Shutdown() error {
	logrus.Info("Shutting down DNS server")
	err := s.dnsServer.Shutdown()

	if err != nil {
		logrus.WithError(err).Errorf("Error shutting down DNS server")
	} else {
		logrus.Info("DNS server has shutdown")
	}

	return err
}

func (s *Server) handleRequest(w dns.ResponseWriter, request *dns.Msg) {
	response := new(dns.Msg)
	response.SetReply(request)
	response.Extra = request.Extra
	response.Authoritative = true
	response.RecursionAvailable = false

	for i, question := range request.Question {
		err := s.handleQuestion(question, request, response)
		if err != nil {
			logrus.WithError(err).Errorf("Error handling DNS question %d: %s", i, question.String())
			// TODO: what should the dns response return ?
			break
		}
	}
	err := w.WriteMsg(response)
	if err != nil {
		logrus.WithError(err).Errorf("Error writing DNS response")
	}
}

func (s *Server) handleQuestion(question dns.Question, request, response *dns.Msg) error {

	switch question.Qclass {
	case dns.ClassINET:
	default:
		response.SetRcode(request, dns.RcodeServerFailure)
		return fmt.Errorf("unsupported DNS question class: %v", dns.Class(question.Qclass).String())
	}

	switch question.Qtype {
	case dns.TypeA:
	case dns.TypeAAAA:
	case dns.TypeSRV:
	default:
		response.SetRcode(request, dns.RcodeServerFailure)
		return fmt.Errorf("unsupported DNS question type: %v", dns.Type(question.Qtype).String())
	}

	serviceInstances, err := s.retrieveServices(question, request, response)

	if err != nil {
		return err
	}
	err = s.createRecordsForInstances(question, request, response, serviceInstances)
	return err

}

func (s *Server) retrieveServices(question dns.Question, request, response *dns.Msg) ([]*client.ServiceInstance, error) {
	var serviceInstances []*client.ServiceInstance
	var err error
	// parse query :
	// Query format:
	// [tag or endpoint type]*.<service>.service.<domain>.
	// <instance_id>.instance.<domain>.
	// For SRV types we also support :
	// _<service>._<tag or endpoint type>.<domain>.

	/// IsDomainName checks if s is a valid domain name
	//  When false is returned the number of labels is not
	// defined.  Also note that this function is extremely liberal; almost any
	// string is a valid domain name as the DNS is 8 bit protocol. It checks if each
	// label fits in 63 characters, but there is no length check for the entire
	// string s. I.e.  a domain name longer than 255 characters is considered valid.
	numberOfLabels, isValidDomain := dns.IsDomainName(question.Name)
	if !isValidDomain {
		response.SetRcode(request, dns.RcodeFormatError)
		return nil, fmt.Errorf("Invalid Domain name %s", question.Name)
	}
	fullDomainRequestArray := dns.SplitDomainName(question.Name)
	if len(fullDomainRequestArray) == 1 || len(fullDomainRequestArray) == 2 {
		response.SetRcode(request, dns.RcodeNameError)
		return nil, fmt.Errorf("service name wasn't included in domain %s", question.Name)
	}
	if fullDomainRequestArray[numberOfLabels-2] == "service" {
		if question.Qtype == dns.TypeSRV && numberOfLabels == 4 &&
			strings.HasPrefix(fullDomainRequestArray[0], "_") &&
			strings.HasPrefix(fullDomainRequestArray[1], "_") {
			// SRV Query :
			tagOrProtocol := fullDomainRequestArray[1][1:]
			serviceName := fullDomainRequestArray[0][1:]
			serviceInstances, err = s.retrieveInstancesForServiceQuery(serviceName, request, response, tagOrProtocol)
		} else {
			serviceName := fullDomainRequestArray[numberOfLabels-3]
			tagsOrProtocol := fullDomainRequestArray[:numberOfLabels-3]
			serviceInstances, err = s.retrieveInstancesForServiceQuery(serviceName, request, response, tagsOrProtocol...)

		}

	} else if fullDomainRequestArray[numberOfLabels-2] == "instance" && (question.Qtype == dns.TypeA ||
		question.Qtype == dns.TypeAAAA) && numberOfLabels == 3 {

		instanceID := fullDomainRequestArray[0]
		serviceInstances, err = s.retrieveInstancesForInstanceQuery(instanceID, request, response)
	}
	return serviceInstances, err
}

func (s *Server) retrieveInstancesForServiceQuery(serviceName string, request, response *dns.Msg, tagOrProtocol ...string) ([]*client.ServiceInstance, error) {
	protocol := ""
	tags := make([]string, 0, len(tagOrProtocol))

	// Split tags and protocol filters
	for _, tag := range tagOrProtocol {
		switch tag {
		case "tcp", "udp", "http", "https":
			if protocol != "" {
				response.SetRcode(request, dns.RcodeFormatError)
				return nil, fmt.Errorf("invalid DNS query: more than one protocol specified")
			}
			protocol = tag
		default:
			tags = append(tags, tag)
		}
	}
	filters := client.InstanceFilter{ServiceName: serviceName, Tags: tags}

	// Dispatch query to registry
	serviceInstances, err := s.config.DiscoveryClient.ListInstances(filters)
	if err != nil {
		response.SetRcode(request, dns.RcodeServerFailure)
		return nil, err
	}

	// Apply protocol filter
	if protocol != "" {
		k := 0
		for _, serviceInstance := range serviceInstances {
			if serviceInstance.Endpoint.Type == protocol {
				serviceInstances[k] = serviceInstance
				k++
			}
		}
		serviceInstances = serviceInstances[:k]
	}

	return serviceInstances, nil
}

func (s *Server) retrieveInstancesForInstanceQuery(instanceID string, request, response *dns.Msg) ([]*client.ServiceInstance, error) {
	serviceInstances, err := s.config.DiscoveryClient.ListInstances(client.InstanceFilter{})
	if err != nil {
		response.SetRcode(request, dns.RcodeServerFailure)
		return serviceInstances, err
	}
	for _, serviceInstance := range serviceInstances {
		if serviceInstance.ID == instanceID {
			return []*client.ServiceInstance{serviceInstance}, nil
		}
	}
	response.SetRcode(request, dns.RcodeNameError)
	return nil, fmt.Errorf("Error : didn't find a service with the id given %s", instanceID)
}

func (s *Server) createRecordsForInstances(question dns.Question, request, response *dns.Msg,
	serviceInstances []*client.ServiceInstance) error {

	answer := make([]dns.RR, 0, 3)
	extra := make([]dns.RR, 0, 3)

	for _, serviceInstance := range serviceInstances {
		ip, port, err := splitHostPort(serviceInstance.Endpoint)
		if err != nil {
			logrus.WithError(err).Warnf("unable to resolve ip address for instance '%s' in DNS query '%s'",
				serviceInstance.ID, question.Name)
			continue
		}

		switch question.Qtype {
		case dns.TypeA:
			ipV4 := ip.To4()
			if ipV4 != nil {
				answer = append(answer, createARecord(question.Name, ipV4))
			}
		case dns.TypeAAAA:
			ipV4 := ip.To4()
			if ipV4 == nil {
				answer = append(answer, createARecord(question.Name, ip.To16()))
			}
		case dns.TypeSRV:
			target := fmt.Sprintf("%s.instance.%s.", serviceInstance.ID, s.config.Domain)
			answer = append(answer, createSRVRecord(question.Name, port, target))

			ipV4 := ip.To4()
			if ipV4 != nil {
				extra = append(extra, createARecord(question.Name, ipV4))
			} else {
				extra = append(extra, createAAAARecord(question.Name, ip.To16()))
			}

		}
	}

	if len(answer) == 0 {
		response.SetRcode(request, dns.RcodeNameError)
		return nil
	}

	// Poor-man's load balancing: randomize returned records order
	shuffleRecords(answer)
	shuffleRecords(extra)

	response.Answer = append(response.Answer, answer...)
	response.Extra = append(response.Extra, extra...)
	response.SetRcode(request, dns.RcodeSuccess)
	return nil

}

func splitHostPort(endpoint client.ServiceEndpoint) (net.IP, uint16, error) {
	switch endpoint.Type {
	case "tcp", "udp":
		return splitHostPortTCPUDP(endpoint.Value)
	case "http", "https":
		return splitHostPortHTTP(endpoint.Value)
	default:
		return nil, 0, fmt.Errorf("unsupported endpoint type: %s", endpoint.Type)
	}
}

func splitHostPortTCPUDP(value string) (net.IP, uint16, error) {
	// Assume value is "host:port"
	host, port, err := net.SplitHostPort(value)

	// Assume value is "host" (no port)
	if err != nil {
		host = value
		port = "0"
	}

	ip := net.ParseIP(host)
	if ip == nil {
		return nil, 0, fmt.Errorf("could not parse '%s' as ip:port", value)
	}

	portNum, err := strconv.Atoi(port)
	if err != nil {
		return nil, 0, err
	}

	return ip, uint16(portNum), nil
}

func splitHostPortHTTP(value string) (net.IP, uint16, error) {
	isHTTP := strings.HasPrefix(value, "http://")
	isHTTPS := strings.HasPrefix(value, "https://")
	if !isHTTPS && !isHTTP {
		value = "http://" + value
		isHTTP = true
	}

	parsedURL, err := url.Parse(value)
	if err != nil {
		return nil, 0, err
	}

	ip, port, err := splitHostPortTCPUDP(parsedURL.Host)
	if err != nil {
		return nil, 0, err
	}

	// Use default port, if not specified
	if port == 0 {
		if isHTTP {
			port = 80
		} else if isHTTPS {
			port = 443
		}
	}

	return ip, port, nil
}

func createARecord(name string, ip net.IP) *dns.A {
	record := &dns.A{
		Hdr: dns.RR_Header{
			Name:   name,
			Rrtype: dns.TypeA,
			Class:  dns.ClassINET,
			Ttl:    0,
		},
		A: ip,
	}
	return record
}

func createAAAARecord(name string, ip net.IP) *dns.AAAA {
	record := &dns.AAAA{
		Hdr: dns.RR_Header{
			Name:   name,
			Rrtype: dns.TypeAAAA,
			Class:  dns.ClassINET,
			Ttl:    0,
		},
		AAAA: ip,
	}
	return record
}

func createSRVRecord(name string, port uint16, target string) *dns.SRV {
	record := &dns.SRV{
		Hdr: dns.RR_Header{
			Name:   name,
			Rrtype: dns.TypeSRV,
			Class:  dns.ClassINET,
			Ttl:    0,
		},
		Port:     port,
		Priority: 0,
		Weight:   0,
		Target:   target,
	}
	return record
}

func validate(config *Config) error {
	if config.DiscoveryClient == nil {
		return fmt.Errorf("Discovery client is nil")
	}

	config.Domain = dns.Fqdn(config.Domain)

	return nil
}

func shuffleRecords(records []dns.RR) {
	for i := range records {
		j := rand.Intn(i + 1)
		records[i], records[j] = records[j], records[i]
	}
}