<?php
declare(strict_types=1);

namespace App\Form;

use App\Form\Data\PasswordResetRequestData;
use Symfony\Component\Form\{AbstractType, Extension\Core\Type\EmailType, FormBuilderInterface};
use EWZ\Bundle\RecaptchaBundle\Form\Type\EWZRecaptchaV3Type;
use Symfony\Component\OptionsResolver\OptionsResolver;

class PasswordResetRequestType extends AbstractType
{
    public function buildForm(FormBuilderInterface $builder, array $options): void
    {
        $builder
            ->add('email', EmailType::class, ['required' => true])
            ->add('recaptcha', EWZRecaptchaV3Type::class)
        ;
    }

    public function configureOptions(OptionsResolver $resolver): void
    {
        $resolver->setDefaults([
            'data_class' => PasswordResetRequestData::class,
        ]);
    }
}
