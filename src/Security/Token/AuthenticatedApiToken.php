<?php
declare(strict_types=1);

namespace App\Security\Token;

use App\Entity\User;
use Symfony\Component\Security\Core\Authentication\Token\PreAuthenticatedToken;
use Symfony\Component\Security\Guard\Token\GuardTokenInterface;

/**
 * This token stores ApiToken key even after eraseCredentials() called
 *
 * @deprecated Refactor to new Authenticators system @see https://gitlab.com/skobkin/magnetico-web/-/issues/26
 */
class AuthenticatedApiToken extends PreAuthenticatedToken implements GuardTokenInterface
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
