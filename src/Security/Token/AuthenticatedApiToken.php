<?php

namespace App\Security\Token;

use App\Entity\User;
use Symfony\Component\Security\Core\Authentication\Token\PreAuthenticatedToken;

/** This token stores ApiToken key even after eraseCredentials() called */
class AuthenticatedApiToken extends PreAuthenticatedToken
{
    /** @var string|null This token is stored only for this request and will not be erased by eraseCredentials() or serialized */
    private $tokenKey;

    public function __construct(User $user, string $credentials, string $providerKey, array $roles = [])
    {
        parent::__construct($user, $credentials, $providerKey, $roles);
        // @todo probably separate constructor argument needed
        $this->tokenKey = $credentials;
    }

    public function getTokenKey(): ?string
    {
        return $this->tokenKey;
    }
}